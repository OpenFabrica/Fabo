package sharetunnel

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/OpenFabrica/Fabo/engine/internal/sharewire"
)

const (
	// idleTimeout: the relay pings every 30s; three missed pings is a dead link.
	idleTimeout    = 90 * time.Second
	connectTimeout = 10 * time.Second
	// LocalDialTimeout bounds connecting to a port target.
	LocalDialTimeout = 3 * time.Second
	// writeTimeout bounds one write to the relay; past it the link is dead.
	writeTimeout = 30 * time.Second
	// reclaim is how long after losing a link its name may still look busy:
	// the relay notices a quiet connection only after 90s, checked every 30s.
	reclaim = 180 * time.Second
	// maxStreams bounds concurrent visitor connections per session, so the
	// worst-case buffering is maxStreams × window per direction.
	maxStreams = 256
	// ctrlQueue bounds queued control frames; a full queue means the relay
	// stopped reading, which ends the session.
	ctrlQueue = 1024
	// dataQueue bounds queued Data/Close frames; senders block on it.
	dataQueue = 64
)

// Request is one visitor request, reported when its stream ends.
type Request struct {
	Method string `json:"method"`
	Path   string `json:"path"`
	// Status is 0 when the local service never answered.
	Status int   `json:"status"`
	MS     int64 `json:"ms"`
	// In is visitor → local service; Out is local service → visitor.
	In  uint64    `json:"in"`
	Out uint64    `json:"out"`
	At  time.Time `json:"at"`
}

// Event types reported through Options.OnEvent.
const (
	EventConnecting   = "connecting"
	EventLive         = "live"
	EventRequest      = "request"
	EventNotice       = "notice"
	EventReconnecting = "reconnecting"
	EventEnded        = "ended"
)

// Event is one change in a tunnel's life; Type selects which fields apply.
type Event struct {
	Type string `json:"type"`
	// Server is set on connecting.
	Server string `json:"server,omitempty"`
	// URL, Rate, Remaining, Warning and Domains are set on live.
	URL       string   `json:"url,omitempty"`
	Rate      uint32   `json:"rate,omitempty"`
	Remaining uint64   `json:"remaining,omitempty"`
	Warning   bool     `json:"warning,omitempty"`
	Domains   []string `json:"domains,omitempty"`
	// Request is set on request.
	Request *Request `json:"request,omitempty"`
	// Text is set on notice.
	Text string `json:"text,omitempty"`
	// Reason is set on reconnecting and ended; DelayMS on reconnecting.
	Reason  string `json:"reason,omitempty"`
	DelayMS int64  `json:"delay_ms,omitempty"`
	// Error is true when the tunnel ended for any reason but Stop.
	Error bool `json:"error,omitempty"`
}

// Options configure one tunnel.
type Options struct {
	Server Server
	Token  string
	Device string
	// Client identifies the program, such as `fabo 0.3.0` or `Fabo 0.3.0`.
	Client string
	Target Target
	// Name is the preferred first label; empty means Target.Label.
	Name string
	// Dial connects to the local service for one visitor connection. It
	// owns its own timeout (see DialAddr).
	Dial func(ctx context.Context) (net.Conn, error)
	// OnEvent gets every event, from the tunnel's goroutines.
	OnEvent func(Event)

	// idle overrides idleTimeout in tests.
	idle time.Duration
}

// DialAddr returns a Dial for a `host:port` target with LocalDialTimeout.
func DialAddr(addr string) func(ctx context.Context) (net.Conn, error) {
	return func(ctx context.Context) (net.Conn, error) {
		ctx, cancel := context.WithTimeout(ctx, LocalDialTimeout)
		defer cancel()
		var d net.Dialer
		conn, err := d.DialContext(ctx, "tcp", addr)
		if tcp, ok := conn.(*net.TCPConn); ok {
			_ = tcp.SetNoDelay(true)
		}
		return conn, err
	}
}

// Handle is a running tunnel.
type Handle struct {
	cancel context.CancelFunc
	done   chan struct{}
	mu     sync.Mutex
	// raw is the live relay socket, closed by Stop to wake every goroutine
	// at once; closing the TLS layer could block on a stalled write.
	raw net.Conn
}

// Start opens a tunnel on its own goroutines and returns at once; it
// reconnects by itself until stopped, refused, or told not to.
func Start(o Options) *Handle {
	if o.idle == 0 {
		o.idle = idleTimeout
	}
	if o.Name == "" {
		o.Name = o.Target.Label
	}
	ctx, cancel := context.WithCancel(context.Background())
	h := &Handle{cancel: cancel, done: make(chan struct{})}
	go h.run(ctx, o)
	return h
}

// Stop closes the tunnel and waits for its goroutines to finish.
func (h *Handle) Stop() {
	h.cancel()
	h.mu.Lock()
	if h.raw != nil {
		h.raw.Close()
	}
	h.mu.Unlock()
	<-h.done
}

// Done is closed once the tunnel has ended and its goroutines are gone.
func (h *Handle) Done() <-chan struct{} { return h.done }

// setRaw records the live socket, closing it at once if Stop came first.
func (h *Handle) setRaw(ctx context.Context, raw net.Conn) {
	h.mu.Lock()
	h.raw = raw
	h.mu.Unlock()
	if raw != nil && ctx.Err() != nil {
		raw.Close()
	}
}

type endKind int

const (
	// endLost: try again after backoff.
	endLost endKind = iota
	// endAgain: the relay is restarting; try again soon.
	endAgain
	// endFinal: don't try again.
	endFinal
)

type end struct {
	kind   endKind
	reason string
}

// backoff is 500ms·2^min(n,6), at most 30s, times a random 50-150% so many
// clients don't reconnect in lockstep.
func backoff(attempt int) time.Duration {
	base := 500 * time.Millisecond << min(attempt, 6)
	base = min(base, 30*time.Second)
	return base * time.Duration(500+rand.IntN(1001)) / 1000
}

// run dials sessions until the tunnel ends for good.
func (h *Handle) run(ctx context.Context, o Options) {
	defer close(h.done)
	emit := func(e Event) {
		if o.OnEvent != nil {
			o.OnEvent(e)
		}
	}
	name := o.Name
	attempt := 0
	var lost time.Time
	finalReason := ""
	for ctx.Err() == nil {
		emit(Event{Type: EventConnecting, Server: o.Server.String()})
		e, issued := h.session(ctx, o, name, emit)
		if issued != "" {
			attempt = 0
			// Asking for the same name again keeps the URL across reconnects.
			name = issued
			lost = time.Now()
		}
		if ctx.Err() != nil {
			break
		}
		reason := e.reason
		var delay time.Duration
		switch e.kind {
		case endFinal:
			// A name refused as busy right after losing the link is usually
			// our own last connection, which the relay hasn't dropped yet.
			if !lost.IsZero() && time.Since(lost) < reclaim && strings.Contains(reason, "already in use") {
				reason = "the link's last connection is still closing"
				delay = max(backoff(attempt), 5*time.Second)
			} else {
				finalReason = reason
			}
		case endAgain:
			delay = 200*time.Millisecond + backoff(0)
		default:
			delay = backoff(attempt)
		}
		if finalReason != "" {
			break
		}
		attempt++
		emit(Event{Type: EventReconnecting, Reason: reason, DelayMS: delay.Milliseconds()})
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
		case <-timer.C:
		}
		timer.Stop()
	}
	if finalReason != "" {
		emit(Event{Type: EventEnded, Reason: finalReason, Error: true})
		return
	}
	emit(Event{Type: EventEnded, Reason: "stopped"})
}

// issuedName is the first label of a Welcome URL's host.
func issuedName(url string) string {
	rest := strings.TrimPrefix(url, "https://")
	if i := strings.IndexAny(rest, ".:/"); i >= 0 {
		return rest[:i]
	}
	return rest
}

// session runs one connection to the relay until it ends; issued is the
// name the relay gave if it welcomed the tunnel.
func (h *Handle) session(ctx context.Context, o Options, name string, emit func(Event)) (end, string) {
	d := net.Dialer{Timeout: connectTimeout}
	raw, err := d.DialContext(ctx, "tcp", o.Server.dialAddr())
	if err != nil {
		return end{endLost, err.Error()}, ""
	}
	if tcp, ok := raw.(*net.TCPConn); ok {
		_ = tcp.SetNoDelay(true)
	}
	h.setRaw(ctx, raw)
	defer func() {
		h.setRaw(ctx, nil)
		raw.Close()
	}()
	conn := tls.Client(raw, o.Server.tlsConfig(sharewire.ALPN))
	hsCtx, cancel := context.WithTimeout(ctx, connectTimeout)
	err = conn.HandshakeContext(hsCtx)
	cancel()
	if err != nil {
		return end{endLost, err.Error()}, ""
	}
	if conn.ConnectionState().NegotiatedProtocol != sharewire.ALPN {
		return end{endFinal, fmt.Sprintf("%s isn't a Fabo share relay", o.Server)}, ""
	}
	sctx, scancel := context.WithCancel(ctx)
	s := &session{
		o: &o, emit: emit, raw: raw, conn: conn, ctx: sctx, cancel: scancel,
		ctrl:    make(chan []byte, ctrlQueue),
		data:    make(chan []byte, dataQueue),
		streams: map[uint32]*sharewire.Stream{},
	}
	s.window.Store(sharewire.InitialWindow)
	s.ctrl <- sharewire.Append(nil, sharewire.Hello{
		Protocol: sharewire.Protocol, Token: o.Token, Device: o.Device, Name: name,
		Target: o.Target.Display, Client: o.Client, Features: sharewire.FeatureWindow,
		Machine: MachineID(o.Device), Host: HostName(),
	})
	s.wg.Add(1)
	go s.writeLoop()
	e := s.readLoop()
	s.shutdown()
	return e, s.issued
}

// session is one connection to the relay and its streams.
type session struct {
	o      *Options
	emit   func(Event)
	raw    net.Conn
	conn   *tls.Conn
	ctx    context.Context
	cancel context.CancelFunc
	// ctrl carries Hello, Pong, Window and Reset ahead of data.
	ctrl chan []byte
	// data carries Data and Close in order per stream.
	data chan []byte
	// window is the client's per-stream send credit, from Welcome.
	window  atomic.Uint32
	issued  string
	mu      sync.Mutex
	streams map[uint32]*sharewire.Stream
	wg      sync.WaitGroup

	failMu     sync.Mutex
	failReason string

	noticeMu   sync.Mutex
	lastNotice time.Time
}

// fail ends the session from any goroutine, keeping the first reason.
func (s *session) fail(reason string) {
	s.failMu.Lock()
	if s.failReason == "" {
		s.failReason = reason
	}
	s.failMu.Unlock()
	s.cancel()
	s.raw.Close()
}

func (s *session) failure() string {
	s.failMu.Lock()
	defer s.failMu.Unlock()
	return s.failReason
}

// SendCtrl queues a control frame without blocking; a full queue means
// the relay stopped reading, which ends the session.
func (s *session) SendCtrl(m sharewire.Msg) bool {
	select {
	case <-s.ctx.Done():
		return false
	default:
	}
	select {
	case s.ctrl <- sharewire.Append(nil, m):
		return true
	default:
		s.fail("the relay stopped reading")
		return false
	}
}

// SendData queues a Data or Close frame, waiting for room.
func (s *session) SendData(m sharewire.Msg) bool {
	frame := sharewire.Append(nil, m)
	select {
	case s.data <- frame:
		return true
	case <-s.ctx.Done():
		return false
	}
}

// writeLoop is the only writer of the TLS connection; control frames go
// before data so Pong and Window never wait behind a slow stream.
func (s *session) writeLoop() {
	defer s.wg.Done()
	for {
		var frame []byte
		select {
		case frame = <-s.ctrl:
		default:
			select {
			case frame = <-s.ctrl:
			case frame = <-s.data:
			case <-s.ctx.Done():
				return
			}
		}
		_ = s.conn.SetWriteDeadline(time.Now().Add(writeTimeout))
		if _, err := s.conn.Write(frame); err != nil {
			s.fail(err.Error())
			return
		}
	}
}

// readLoop handles frames until the session ends; it never blocks on a
// stream, only on the relay socket.
func (s *session) readLoop() end {
	br := bufio.NewReaderSize(s.conn, 64*1024)
	var buf []byte
	for {
		_ = s.conn.SetReadDeadline(time.Now().Add(s.o.idle))
		frame, err := sharewire.ReadFrame(br, buf)
		if err != nil {
			if r := s.failure(); r != "" {
				return end{endLost, r}
			}
			var ne net.Error
			switch {
			case errors.As(err, &ne) && ne.Timeout():
				return end{endLost, "the relay stopped answering"}
			case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
				return end{endLost, "the relay closed the connection"}
			default:
				return end{endLost, err.Error()}
			}
		}
		buf = frame
		m, err := sharewire.Decode(frame)
		if err != nil {
			return end{endLost, "bad frame from the relay: " + err.Error()}
		}
		if e, done := s.handle(m); done {
			return e
		}
	}
}

// handle acts on one frame; done ends the session.
func (s *session) handle(m sharewire.Msg) (end, bool) {
	switch m := m.(type) {
	case sharewire.Welcome:
		s.issued = issuedName(m.URL)
		s.window.Store(m.SendWindow())
		var domains []string
		for _, d := range strings.Split(m.Domains, ",") {
			if d = strings.TrimSpace(d); d != "" {
				domains = append(domains, d)
			}
		}
		s.emit(Event{Type: EventLive, URL: m.URL, Rate: m.Rate, Remaining: m.Remaining, Warning: m.Warning, Domains: domains})
	case sharewire.Refused:
		return end{endFinal, m.Reason}, true
	case sharewire.Open:
		s.open(m.Stream)
	case sharewire.Data:
		if st := s.lookup(m.Stream); st != nil && !st.Push(m.Data) {
			// More than the window we granted: the relay broke flow control.
			st.Reset()
		}
	case sharewire.Window:
		if st := s.lookup(m.Stream); st != nil && !st.Grant(m.Credit) {
			return end{endLost, fmt.Sprintf("bad credit %d from the relay", m.Credit)}, true
		}
	case sharewire.Close:
		if st := s.lookup(m.Stream); st != nil {
			st.PushEOF()
		}
	case sharewire.Reset:
		if st := s.remove(m.Stream); st != nil {
			st.Kill()
		}
	case sharewire.Ping:
		if !s.SendCtrl(sharewire.Pong{Value: m.Value}) {
			return end{endLost, s.failure()}, true
		}
	case sharewire.Pong:
	case sharewire.Notice:
		s.emit(Event{Type: EventNotice, Text: m.Text})
	case sharewire.Goodbye:
		if m.Reconnect {
			return end{endAgain, m.Reason}, true
		}
		return end{endFinal, m.Reason}, true
	default:
		return end{endLost, "unexpected frame from the relay"}, true
	}
	return end{}, false
}

func (s *session) lookup(id uint32) *sharewire.Stream {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.streams[id]
}

func (s *session) remove(id uint32) *sharewire.Stream {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.streams[id]
	delete(s.streams, id)
	return st
}

// open registers a visitor stream at once (data may arrive before the
// local connection is up) and connects on its own goroutine. The client
// sends within Welcome's window and accepts the fixed InitialWindow.
func (s *session) open(id uint32) {
	s.mu.Lock()
	if _, dup := s.streams[id]; dup || len(s.streams) >= maxStreams {
		s.mu.Unlock()
		s.SendCtrl(sharewire.Reset{Stream: id})
		return
	}
	st := sharewire.NewStream(id, s, s.window.Load(), sharewire.InitialWindow)
	st.OnReset = func() { s.remove(id) }
	s.streams[id] = st
	s.mu.Unlock()
	s.wg.Add(1)
	go s.runStream(st)
}

// shutdown ends the session: wakes every goroutine and waits for them.
func (s *session) shutdown() {
	s.cancel()
	s.raw.Close()
	s.mu.Lock()
	streams := s.streams
	s.streams = map[uint32]*sharewire.Stream{}
	s.mu.Unlock()
	for _, st := range streams {
		st.Kill()
	}
	s.wg.Wait()
}

// notice reports an unreachable local service at most every 5s.
func (s *session) notice(err error) {
	s.noticeMu.Lock()
	defer s.noticeMu.Unlock()
	if time.Since(s.lastNotice) < 5*time.Second {
		return
	}
	s.lastNotice = time.Now()
	where := s.o.Target.Addr
	if where == "" {
		where = s.o.Target.Display
	}
	s.emit(Event{Type: EventNotice, Text: fmt.Sprintf("nothing is answering on %s (%v)", where, err)})
}

// firstChunk waits up to d for the stream's first relay→local bytes.
func (s *session) firstChunk(st *sharewire.Stream, d time.Duration) []byte {
	deadline := time.Now().Add(d)
	for {
		if first := st.FirstIn(); first != nil {
			return first
		}
		if time.Now().After(deadline) || s.ctx.Err() != nil {
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// runStream connects to the local service and carries both directions
// until both have ended or the stream is reset, then reports the request.
func (s *session) runStream(st *sharewire.Stream) {
	defer s.wg.Done()
	started := time.Now()
	local, err := s.o.Dial(s.ctx)
	if err != nil {
		// The request line has usually arrived (or arrives within moments);
		// wait for it while the stream still accepts data, then reset and
		// log it as unanswered.
		first := s.firstChunk(st, 200*time.Millisecond)
		st.Reset()
		if method, path, ok := parseRequestLine(first); ok {
			s.emit(Event{Type: EventRequest, Request: &Request{Method: method, Path: path, MS: time.Since(started).Milliseconds(), In: st.In.Load(), At: started}})
		}
		if s.ctx.Err() == nil {
			s.notice(err)
		}
		return
	}
	st.Pump(local)
	s.remove(st.ID)
	if method, path, ok := parseRequestLine(st.FirstIn()); ok {
		status, _ := parseStatusLine(st.FirstOut())
		s.emit(Event{Type: EventRequest, Request: &Request{Method: method, Path: path, Status: status,
			MS: time.Since(started).Milliseconds(), In: st.In.Load(), Out: st.Out.Load(), At: started}})
	}
}
