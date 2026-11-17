package main

// Debug sessions at run time: the sockets of one session and what passed
// through them. Each running session owns a context and a set of
// goroutines; everything the page reads is behind the run's mutex and
// copied out by snapshot, so the view never waits on the network.

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	mrand "math/rand/v2"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/coder/websocket"
)

// Run states.
const (
	debugIdle      = ""
	debugBusy      = "connecting"
	debugOpen      = "open"
	debugListening = "listening"
	debugFailed    = "failed"
)

// Line directions.
const (
	lineSent     = '>'
	lineReceived = '<'
	lineNote     = '*'
	lineError    = '!'
)

const (
	debugMaxLines     = 5000
	debugMaxExchanges = 50
	debugMaxHits      = 200
	debugReadLimit    = 16 << 20
)

// debugLine is one message or event of a session.
type debugLine struct {
	At   time.Time
	Dir  byte
	Peer string
	Data []byte
	Note string
	Auto bool
}

// debugPeer is one client of a server session: a connection, or for UDP an
// address that sent a datagram.
type debugPeer struct {
	Addr   string
	Since  time.Time
	Gone   bool
	Picked bool
	send   func(data []byte, binary bool) error
	close  func()
}

// httpExchange is one request an HTTP client session made.
type httpExchange struct {
	At       time.Time
	Method   string
	URL      string
	Status   int
	Proto    string
	Duration time.Duration
	Headers  http.Header
	Body     []byte
	Err      string
}

// mockHit is one request an HTTP mock answered.
type mockHit struct {
	At       time.Time
	Method   string
	Path     string
	Peer     string
	Status   int
	Duration time.Duration
}

// dialFunc opens a client's connection; the page picks it from the
// session's target (directly, or through the engine for a container).
type dialFunc func(ctx context.Context, network, address string) (net.Conn, error)

// debugRun is the live state of one session.
type debugRun struct {
	mu        sync.Mutex
	state     string
	problem   string
	since     time.Time
	lines     []debugLine
	peers     []*debugPeer
	exchanges []httpExchange
	hits      []mockHit
	routeHits map[int]int
	// live is the session as last saved while the run holds sockets, so a
	// server's reply settings and a mock's routes apply without a restart.
	live *debugSession
	// send writes to a client's connection while it is open.
	send   func(data []byte, binary bool) error
	cancel context.CancelFunc
	done   chan struct{}
	notify func()
}

// debugSnap is a copy of a run for one frame of the view.
type debugSnap struct {
	state, problem string
	since          time.Time
	lines          []debugLine
	peers          []debugPeer
	exchanges      []httpExchange
	hits           []mockHit
	routeHits      map[int]int
}

// newDebugRun makes the run of a session; notify asks the window to redraw.
func newDebugRun(notify func()) *debugRun {
	if notify == nil {
		notify = func() {}
	}
	return &debugRun{notify: notify, routeHits: map[int]int{}}
}

// snapshot copies what the view shows. Lines are never changed once added,
// so sharing their backing array is safe.
func (r *debugRun) snapshot() debugSnap {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := debugSnap{state: r.state, problem: r.problem, since: r.since, lines: r.lines, exchanges: r.exchanges, hits: r.hits,
		routeHits: make(map[int]int, len(r.routeHits))}
	for _, p := range r.peers {
		out.peers = append(out.peers, *p)
	}
	for k, v := range r.routeHits {
		out.routeHits[k] = v
	}
	return out
}

// active reports whether the run holds sockets.
func (r *debugRun) active() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.done != nil
}

// add appends a line, dropping the oldest beyond the limit. The slice is
// copied when trimmed so a snapshot taken earlier keeps its lines.
func (r *debugRun) add(l debugLine) {
	if l.At.IsZero() {
		l.At = time.Now()
	}
	r.mu.Lock()
	if len(r.lines) >= debugMaxLines {
		keep := r.lines[len(r.lines)-debugMaxLines/2:]
		r.lines = append(make([]debugLine, 0, debugMaxLines), keep...)
	}
	r.lines = append(r.lines, l)
	r.mu.Unlock()
	r.notify()
}

// note adds an event line.
func (r *debugRun) note(text string) { r.add(debugLine{Dir: lineNote, Note: text}) }

// settings is the session as it is now: the live copy when one was set
// since the run started, else the copy it started with.
func (r *debugRun) settings(x debugSession) debugSession {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.live != nil {
		return *r.live
	}
	return x
}

// fail ends the run's state in an error, which the page shows.
func (r *debugRun) fail(err error) {
	r.mu.Lock()
	r.state, r.problem = debugFailed, err.Error()
	r.mu.Unlock()
	r.add(debugLine{Dir: lineError, Note: err.Error()})
}

// setState moves the run to state, starting its clock.
func (r *debugRun) setState(state string) {
	r.mu.Lock()
	r.state, r.problem, r.since = state, "", time.Now()
	r.mu.Unlock()
	r.notify()
}

// clear forgets the messages, keeping the connections.
func (r *debugRun) clear() {
	r.mu.Lock()
	r.lines, r.exchanges, r.hits = nil, nil, nil
	r.routeHits = map[int]int{}
	r.mu.Unlock()
	r.notify()
}

// stop closes the run's sockets and waits for its goroutines, so the same
// port can be listened on again at once.
func (r *debugRun) stop() {
	r.mu.Lock()
	cancel, done := r.cancel, r.done
	r.mu.Unlock()
	if cancel == nil {
		return
	}
	cancel()
	<-done
}

// begin marks the run started; it fails if it already is.
func (r *debugRun) begin(parent context.Context) (context.Context, chan struct{}, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.done != nil {
		return nil, nil, false
	}
	ctx, cancel := context.WithCancel(parent)
	r.cancel, r.done, r.live = cancel, make(chan struct{}), nil
	r.state, r.problem, r.since = debugBusy, "", time.Now()
	return ctx, r.done, true
}

// end marks the run stopped once its goroutines are done.
func (r *debugRun) end(done chan struct{}) {
	r.mu.Lock()
	r.cancel, r.done, r.send = nil, nil, nil
	if r.state != debugFailed {
		r.state = debugIdle
	}
	for _, p := range r.peers {
		p.Gone = true
	}
	r.mu.Unlock()
	close(done)
	r.notify()
}

// start opens a session's sockets in the background. dial is used by
// clients; servers ignore it.
func (r *debugRun) start(parent context.Context, x debugSession, dial dialFunc) {
	ctx, done, ok := r.begin(parent)
	if !ok {
		return
	}
	r.notify()
	go func() {
		defer r.end(done)
		var err error
		switch {
		case x.Kind == debugClient && x.Proto == debugWS:
			err = r.runWSClient(ctx, x, dial)
		case x.Kind == debugClient:
			err = r.runClient(ctx, x, dial)
		case x.Proto == debugTCP:
			err = r.runTCPServer(ctx, x)
		case x.Proto == debugUDP:
			err = r.runUDPServer(ctx, x)
		case x.Proto == debugWS:
			err = r.runWSServer(ctx, x)
		case x.Proto == debugHTTP:
			err = r.runMock(ctx, x)
		}
		if err != nil && ctx.Err() == nil {
			r.fail(err)
		}
	}()
}

// sendClient writes data to an open client connection.
func (r *debugRun) sendClient(data []byte, binary bool) error {
	r.mu.Lock()
	send := r.send
	r.mu.Unlock()
	if send == nil {
		return errors.New(tr("未连接", "Not connected"))
	}
	if err := send(data, binary); err != nil {
		return err
	}
	r.add(debugLine{Dir: lineSent, Data: data})
	return nil
}

// sendPeers writes data to the server's picked peers and reports how many
// got it.
func (r *debugRun) sendPeers(data []byte, binary bool) (int, error) {
	r.mu.Lock()
	var targets []*debugPeer
	for _, p := range r.peers {
		if p.Picked && !p.Gone && p.send != nil {
			targets = append(targets, p)
		}
	}
	r.mu.Unlock()
	if len(targets) == 0 {
		return 0, errors.New(tr("没有选中的连接", "No connection is picked"))
	}
	sent := 0
	var firstErr error
	for _, p := range targets {
		if err := p.send(data, binary); err != nil {
			firstErr = cmpErr(firstErr, err)
			continue
		}
		sent++
		r.add(debugLine{Dir: lineSent, Peer: p.Addr, Data: data})
	}
	return sent, firstErr
}

// cmpErr keeps the first error.
func cmpErr(first, next error) error {
	if first != nil {
		return first
	}
	return next
}

// pick ticks or unticks a server's peer as a target of what is sent.
func (r *debugRun) pick(addr string, on bool) {
	r.mu.Lock()
	for _, p := range r.peers {
		if p.Addr == addr {
			p.Picked = on
		}
	}
	r.mu.Unlock()
	r.notify()
}

// dropGone forgets the peers that have gone.
func (r *debugRun) dropGone() {
	r.mu.Lock()
	keep := r.peers[:0]
	for _, p := range r.peers {
		if !p.Gone {
			keep = append(keep, p)
		}
	}
	r.peers = keep
	r.mu.Unlock()
	r.notify()
}

// addPeer records a new peer; the first one is picked, so a single
// client can be answered without ticking it.
func (r *debugRun) addPeer(p *debugPeer) {
	r.mu.Lock()
	picked := false
	for _, q := range r.peers {
		if q.Picked && !q.Gone {
			picked = true
		}
	}
	p.Picked = !picked
	p.Since = time.Now()
	r.peers = append(r.peers, p)
	r.mu.Unlock()
}

// leave marks a peer gone.
func (r *debugRun) leave(p *debugPeer) {
	r.mu.Lock()
	p.Gone = true
	r.mu.Unlock()
}

// reply is a server's automatic answer to data: the configured reply, or
// the data itself when the reply is empty (an echo).
func reply(x debugSession, data []byte) []byte {
	if strings.TrimSpace(x.Reply) == "" {
		return data
	}
	out, err := encodeText(x.Reply, x.Encoding, x.NoEscape)
	if err != nil {
		return data
	}
	return out
}

// greeting is the bytes a server sends a client as it connects, if any.
func greeting(x debugSession) []byte {
	if x.Greeting == "" {
		return nil
	}
	out, err := encodeText(x.Greeting, x.Encoding, x.NoEscape)
	if err != nil {
		return nil
	}
	return out
}

// clientTarget is the address a client connects to.
func clientTarget(x debugSession) string {
	if x.Container != "" {
		target := fmt.Sprintf("%s:%d", x.Container, x.Port)
		// A WebSocket or HTTP target keeps its path in Address.
		if path := strings.TrimSpace(x.Address); path != "" && (x.Proto == debugWS || x.Proto == debugHTTP) {
			if !strings.HasPrefix(path, "/") {
				path = "/" + path
			}
			target += path
		}
		return target
	}
	return strings.TrimSpace(x.Address)
}

// runClient keeps a TCP or UDP client's connection until it ends.
func (r *debugRun) runClient(ctx context.Context, x debugSession, dial dialFunc) error {
	target := clientTarget(x)
	dialCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	conn, err := dial(dialCtx, x.Proto, target)
	cancel()
	if err != nil {
		return fmt.Errorf(tr("连接失败：%v", "Could not connect: %v"), err)
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	defer conn.Close()
	r.mu.Lock()
	r.send = func(data []byte, _ bool) error {
		_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
		_, err := conn.Write(data)
		return err
	}
	r.mu.Unlock()
	r.setState(debugOpen)
	r.note(fmt.Sprintf(tr("已连接 %s", "Connected to %s"), target))
	buf := make([]byte, 64<<10)
	for {
		n, err := conn.Read(buf)
		if n > 0 {
			r.add(debugLine{Dir: lineReceived, Data: bytes.Clone(buf[:n])})
		}
		if err != nil {
			if ctx.Err() != nil {
				r.note(tr("已断开", "Disconnected"))
				return nil
			}
			if errors.Is(err, io.EOF) {
				r.note(tr("对端已关闭连接", "The peer closed the connection"))
				return nil
			}
			// A UDP socket reports an unreachable port on the next read;
			// it stays usable, so say so and keep reading.
			if x.Proto == debugUDP && isRefused(err) {
				r.add(debugLine{Dir: lineError, Note: tr("目标端口不可达", "The target port is unreachable")})
				continue
			}
			return err
		}
	}
}

// isRefused reports a refused connection or an unreachable UDP port.
func isRefused(err error) bool {
	return strings.Contains(err.Error(), "connection refused")
}

// runWSClient keeps a WebSocket client's connection until it ends.
func (r *debugRun) runWSClient(ctx context.Context, x debugSession, dial dialFunc) error {
	target := clientTarget(x)
	if !strings.Contains(target, "://") {
		target = "ws://" + target
	}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) { return dial(ctx, "tcp", addr) },
		// Certificates are checked unless the session says not to, for a
		// development server with a self-signed one.
		TLSClientConfig: &tls.Config{InsecureSkipVerify: x.Insecure},
	}
	defer transport.CloseIdleConnections()
	dialCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	conn, _, err := websocket.Dial(dialCtx, target, &websocket.DialOptions{HTTPClient: &http.Client{Transport: transport}})
	cancel()
	if err != nil {
		return fmt.Errorf(tr("连接失败：%v", "Could not connect: %v"), err)
	}
	conn.SetReadLimit(debugReadLimit)
	defer conn.CloseNow()
	r.mu.Lock()
	r.send = func(data []byte, binary bool) error { return wsWrite(ctx, conn, data, binary) }
	r.mu.Unlock()
	r.setState(debugOpen)
	r.note(fmt.Sprintf(tr("已连接 %s", "Connected to %s"), target))
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			if ctx.Err() != nil {
				_ = conn.Close(websocket.StatusNormalClosure, "")
				r.note(tr("已断开", "Disconnected"))
				return nil
			}
			if status := websocket.CloseStatus(err); status != -1 {
				r.note(fmt.Sprintf(tr("对端已关闭连接（%d）", "The peer closed the connection (%d)"), status))
				return nil
			}
			return err
		}
		r.add(debugLine{Dir: lineReceived, Data: data})
	}
}

// wsWrite sends one WebSocket message: text when it is valid UTF-8 and was
// typed as text, binary otherwise.
func wsWrite(ctx context.Context, conn *websocket.Conn, data []byte, binary bool) error {
	kind := websocket.MessageText
	if binary || !utf8.Valid(data) {
		kind = websocket.MessageBinary
	}
	writeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return conn.Write(writeCtx, kind, data)
}

// listen opens a server's port with a message the page can show.
func listen(network, address string) (net.Listener, error) {
	ln, err := net.Listen(network, address)
	if err != nil {
		return nil, listenError(err)
	}
	return ln, nil
}

// listenError explains the usual reasons a port cannot be opened.
func listenError(err error) error {
	switch {
	case strings.Contains(err.Error(), "address already in use"):
		return errors.New(tr("端口已被占用", "The port is already in use"))
	case strings.Contains(err.Error(), "permission denied"):
		return errors.New(tr("没有权限：小于 1024 的端口需要管理员权限", "Permission denied: ports below 1024 need administrator rights"))
	}
	return fmt.Errorf(tr("监听失败：%v", "Could not listen: %v"), err)
}

// listenAll opens a server's port on each of its addresses, closing the
// ones already open when one fails.
func listenAll(network string, addrs []string) ([]net.Listener, error) {
	var out []net.Listener
	for _, addr := range addrs {
		ln, err := listen(network, addr)
		if err != nil {
			for _, open := range out {
				_ = open.Close()
			}
			return nil, err
		}
		out = append(out, ln)
	}
	return out, nil
}

// runTCPServer accepts TCP clients on every address until the session
// stops.
func (r *debugRun) runTCPServer(ctx context.Context, x debugSession) error {
	addrs := x.listenAddresses()
	listeners, err := listenAll("tcp", addrs)
	if err != nil {
		return err
	}
	stop := context.AfterFunc(ctx, func() {
		for _, ln := range listeners {
			_ = ln.Close()
		}
	})
	defer stop()
	r.setState(debugListening)
	r.note(fmt.Sprintf(tr("开始监听 %s", "Listening on %s"), strings.Join(addrs, ", ")))
	var conns sync.WaitGroup
	defer conns.Wait()
	errs := make(chan error, len(listeners))
	for _, ln := range listeners {
		go func() {
			for {
				conn, err := ln.Accept()
				if err != nil {
					errs <- err
					return
				}
				conns.Go(func() { r.serveTCP(ctx, x, conn) })
			}
		}()
	}
	// The first listener to fail ends the server; stopping closes them all.
	err = <-errs
	for _, ln := range listeners {
		_ = ln.Close()
	}
	for range len(listeners) - 1 {
		<-errs
	}
	if ctx.Err() != nil {
		r.note(tr("已停止监听", "Stopped listening"))
		return nil
	}
	return err
}

// serveTCP is one client of a TCP server.
func (r *debugRun) serveTCP(ctx context.Context, x debugSession, conn net.Conn) {
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	var writeMu sync.Mutex
	write := func(data []byte, _ bool) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
		_, err := conn.Write(data)
		return err
	}
	peer := &debugPeer{Addr: conn.RemoteAddr().String(), send: write, close: func() { _ = conn.Close() }}
	r.addPeer(peer)
	r.add(debugLine{Dir: lineNote, Peer: peer.Addr, Note: tr("已连接", "connected")})
	if hello := greeting(x); hello != nil && write(hello, false) == nil {
		r.add(debugLine{Dir: lineSent, Peer: peer.Addr, Data: hello, Auto: true})
	}
	buf := make([]byte, 64<<10)
	for {
		n, err := conn.Read(buf)
		if n > 0 {
			data := bytes.Clone(buf[:n])
			r.add(debugLine{Dir: lineReceived, Peer: peer.Addr, Data: data})
			if cfg := r.settings(x); cfg.AutoReply {
				out := reply(cfg, data)
				if write(out, false) == nil {
					r.add(debugLine{Dir: lineSent, Peer: peer.Addr, Data: out, Auto: true})
				}
			}
		}
		if err != nil {
			r.leave(peer)
			if ctx.Err() == nil {
				r.add(debugLine{Dir: lineNote, Peer: peer.Addr, Note: tr("已断开", "disconnected")})
			}
			return
		}
	}
}

// runUDPServer receives datagrams on every address until the session
// stops. A peer is an address that sent something; replies go back to it
// from the socket it sent to.
func (r *debugRun) runUDPServer(ctx context.Context, x debugSession) error {
	addrs := x.listenAddresses()
	var sockets []net.PacketConn
	for _, addr := range addrs {
		pc, err := net.ListenPacket("udp", addr)
		if err != nil {
			for _, open := range sockets {
				_ = open.Close()
			}
			return listenError(err)
		}
		sockets = append(sockets, pc)
	}
	stop := context.AfterFunc(ctx, func() {
		for _, pc := range sockets {
			_ = pc.Close()
		}
	})
	defer stop()
	r.setState(debugListening)
	r.note(fmt.Sprintf(tr("开始监听 %s", "Listening on %s"), strings.Join(addrs, ", ")))
	var mu sync.Mutex
	peers := map[string]*debugPeer{}
	errs := make(chan error, len(sockets))
	for _, pc := range sockets {
		go func() {
			buf := make([]byte, 64<<10)
			for {
				n, addr, err := pc.ReadFrom(buf)
				if err != nil {
					errs <- err
					return
				}
				key := addr.String()
				mu.Lock()
				peer := peers[key]
				if peer == nil {
					to := addr
					peer = &debugPeer{Addr: key, send: func(data []byte, _ bool) error {
						_, err := pc.WriteTo(data, to)
						return err
					}}
					peers[key] = peer
					r.addPeer(peer)
				}
				mu.Unlock()
				data := bytes.Clone(buf[:n])
				r.add(debugLine{Dir: lineReceived, Peer: key, Data: data})
				if cfg := r.settings(x); cfg.AutoReply {
					out := reply(cfg, data)
					if peer.send(out, false) == nil {
						r.add(debugLine{Dir: lineSent, Peer: key, Data: out, Auto: true})
					}
				}
			}
		}()
	}
	err := <-errs
	for _, pc := range sockets {
		_ = pc.Close()
	}
	for range len(sockets) - 1 {
		<-errs
	}
	if ctx.Err() != nil {
		r.note(tr("已停止监听", "Stopped listening"))
		return nil
	}
	return err
}

// serveHTTP runs an HTTP server on every address of a session, with TLS
// when asked, until the session stops.
func (r *debugRun) serveHTTP(ctx context.Context, x debugSession, handler http.Handler, certDir string) error {
	addrs := x.listenAddresses()
	listeners, err := listenAll("tcp", addrs)
	if err != nil {
		return err
	}
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second,
		BaseContext: func(net.Listener) context.Context { return ctx }}
	if x.TLS {
		cert, err := debugCertificate(x, certDir)
		if err != nil {
			for _, ln := range listeners {
				_ = ln.Close()
			}
			return err
		}
		server.TLSConfig = &tls.Config{Certificates: []tls.Certificate{cert}}
		for i, ln := range listeners {
			listeners[i] = tls.NewListener(ln, server.TLSConfig)
		}
	}
	stop := context.AfterFunc(ctx, func() {
		shutdown, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
		_ = server.Close()
	})
	defer stop()
	r.setState(debugListening)
	r.note(fmt.Sprintf(tr("开始监听 %s", "Listening on %s"), strings.Join(addrs, ", ")))
	errs := make(chan error, len(listeners))
	for _, ln := range listeners {
		go func() { errs <- server.Serve(ln) }()
	}
	err = <-errs
	_ = server.Close()
	for range len(listeners) - 1 {
		<-errs
	}
	if ctx.Err() != nil || errors.Is(err, http.ErrServerClosed) {
		r.note(tr("已停止监听", "Stopped listening"))
		return nil
	}
	return err
}

// debugCertDir is where the self-signed certificate of TLS servers lives;
// the page sets it beside the sessions file.
var debugCertDir = ""

// runWSServer accepts WebSocket clients on any path until the session stops.
func (r *debugRun) runWSServer(ctx context.Context, x debugSession) error {
	var conns sync.WaitGroup
	defer conns.Wait()
	handler := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		// Any origin may connect: this is a test server on a developer's
		// machine, and browsers' pages are among its clients.
		conn, err := websocket.Accept(w, req, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		conns.Add(1)
		defer conns.Done()
		r.serveWS(ctx, x, conn, req.RemoteAddr)
	})
	return r.serveHTTP(ctx, x, handler, debugCertDir)
}

// serveWS is one client of a WebSocket server.
func (r *debugRun) serveWS(ctx context.Context, x debugSession, conn *websocket.Conn, addr string) {
	defer conn.CloseNow()
	conn.SetReadLimit(debugReadLimit)
	write := func(data []byte, binary bool) error { return wsWrite(ctx, conn, data, binary) }
	peer := &debugPeer{Addr: addr, send: write, close: func() { _ = conn.Close(websocket.StatusNormalClosure, "") }}
	r.addPeer(peer)
	r.add(debugLine{Dir: lineNote, Peer: addr, Note: tr("已连接", "connected")})
	if hello := greeting(x); hello != nil && write(hello, false) == nil {
		r.add(debugLine{Dir: lineSent, Peer: addr, Data: hello, Auto: true})
	}
	for {
		kind, data, err := conn.Read(ctx)
		if err != nil {
			r.leave(peer)
			if ctx.Err() == nil {
				r.add(debugLine{Dir: lineNote, Peer: addr, Note: tr("已断开", "disconnected")})
			}
			return
		}
		r.add(debugLine{Dir: lineReceived, Peer: addr, Data: data})
		if cfg := r.settings(x); cfg.AutoReply {
			out := reply(cfg, data)
			if write(out, kind == websocket.MessageBinary) == nil {
				r.add(debugLine{Dir: lineSent, Peer: addr, Data: out, Auto: true})
			}
		}
	}
}

// debugCertificate is a TLS server's certificate: the configured PEM files,
// or a self-signed one for localhost made once and kept in dir.
func debugCertificate(x debugSession, dir string) (tls.Certificate, error) {
	if x.CertFile != "" || x.KeyFile != "" {
		cert, err := tls.LoadX509KeyPair(x.CertFile, x.KeyFile)
		if err != nil {
			return tls.Certificate{}, fmt.Errorf(tr("证书或私钥无效：%v", "The certificate or key is not valid: %v"), err)
		}
		return cert, nil
	}
	if dir == "" {
		dir = os.TempDir()
	}
	certPath, keyPath := filepath.Join(dir, "debug-cert.pem"), filepath.Join(dir, "debug-key.pem")
	if cert, err := tls.LoadX509KeyPair(certPath, keyPath); err == nil {
		return cert, nil
	}
	certPEM, keyPEM, err := selfSigned()
	if err != nil {
		return tls.Certificate{}, err
	}
	if err := os.MkdirAll(dir, 0o700); err == nil {
		_ = os.WriteFile(keyPath, keyPEM, 0o600)
		_ = os.WriteFile(certPath, certPEM, 0o600)
	}
	return tls.X509KeyPair(certPEM, keyPEM)
}

// selfSigned makes a certificate for localhost and host.docker.internal.
func selfSigned() (certPEM, keyPEM []byte, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	if err != nil {
		return nil, nil, err
	}
	template := x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "Fabo debug server"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().AddDate(5, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"localhost", "host.docker.internal", "host.fabo.internal"},
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback},
	}
	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		return nil, nil, err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), nil
}

// runMock answers HTTP requests from the session's routes until it stops.
func (r *debugRun) runMock(ctx context.Context, x debugSession) error {
	handler := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		started := time.Now()
		status := r.answerMock(ctx, x, w, req)
		r.mu.Lock()
		if len(r.hits) >= debugMaxHits {
			r.hits = append([]mockHit(nil), r.hits[len(r.hits)-debugMaxHits/2:]...)
		}
		r.hits = append(r.hits, mockHit{At: started, Method: req.Method, Path: req.URL.RequestURI(), Peer: req.RemoteAddr,
			Status: status, Duration: time.Since(started)})
		r.mu.Unlock()
		r.notify()
	})
	return r.serveHTTP(ctx, x, handler, debugCertDir)
}

// answerMock serves one request and returns the status it sent.
func (r *debugRun) answerMock(ctx context.Context, x debugSession, w http.ResponseWriter, req *http.Request) int {
	routes := r.settings(x).Routes
	i, params, ok := matchRoute(routes, req.Method, req.URL.Path)
	if !ok {
		http.Error(w, "no route matches "+req.Method+" "+req.URL.Path, http.StatusNotFound)
		return http.StatusNotFound
	}
	route := routes[i]
	r.mu.Lock()
	r.routeHits[i]++
	r.mu.Unlock()
	if route.Delay > 0 {
		select {
		case <-time.After(time.Duration(route.Delay) * time.Millisecond):
		case <-ctx.Done():
			return 0
		case <-req.Context().Done():
			return 0
		}
	}
	if route.Dir != "" {
		// The part of the path the wildcard matched names the file.
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		file := params["*"]
		http.ServeFile(rec, req, filepath.Join(route.Dir, filepath.FromSlash(filepath.Clean("/"+file))))
		return rec.status
	}
	status := route.Status
	if status == 0 {
		status = http.StatusOK
	}
	if route.ContentType != "" {
		w.Header().Set("Content-Type", route.ContentType)
	}
	w.WriteHeader(status)
	_, _ = io.WriteString(w, expandTemplate(route.Body, params))
	return status
}

// statusRecorder notes the status a handler wrote.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

// matchRoute finds the first route for method and path. A route's path may
// hold :name segments and end in *, which matches the rest; ANY or an
// empty method matches every method.
func matchRoute(routes []debugRoute, method, path string) (int, map[string]string, bool) {
	for i, route := range routes {
		m := strings.ToUpper(strings.TrimSpace(route.Method))
		if m != "" && m != "ANY" && m != method {
			continue
		}
		if params, ok := matchPath(route.Path, path); ok {
			return i, params, true
		}
	}
	return 0, nil, false
}

// matchPath matches a route pattern against a request path.
func matchPath(pattern, path string) (map[string]string, bool) {
	want := strings.Split(strings.Trim(pattern, "/"), "/")
	got := strings.Split(strings.Trim(path, "/"), "/")
	params := map[string]string{}
	for i, part := range want {
		if part == "*" && i == len(want)-1 {
			params["*"] = strings.Join(got[min(i, len(got)):], "/")
			return params, true
		}
		if i >= len(got) {
			return nil, false
		}
		switch {
		case strings.HasPrefix(part, ":") && got[i] != "":
			params[part[1:]] = got[i]
		case part != got[i]:
			return nil, false
		}
	}
	return params, len(want) == len(got)
}

// expandTemplate fills {{path.name}}, {{uuid}} and {{rand}} in a mock's body.
func expandTemplate(body string, params map[string]string) string {
	if !strings.Contains(body, "{{") {
		return body
	}
	var b strings.Builder
	for {
		start := strings.Index(body, "{{")
		if start < 0 {
			b.WriteString(body)
			return b.String()
		}
		end := strings.Index(body[start:], "}}")
		if end < 0 {
			b.WriteString(body)
			return b.String()
		}
		b.WriteString(body[:start])
		name := strings.TrimSpace(body[start+2 : start+end])
		switch {
		case name == "uuid":
			b.WriteString(newUUID())
		case name == "rand":
			fmt.Fprintf(&b, "%08x", mrand.Uint32())
		case strings.HasPrefix(name, "path."):
			b.WriteString(params[strings.TrimPrefix(name, "path.")])
		default:
			b.WriteString(body[start : start+end+2])
		}
		body = body[start+end+2:]
	}
}

// newUUID is a random version 4 UUID.
func newUUID() string {
	var u [16]byte
	_, _ = rand.Read(u[:])
	u[6] = u[6]&0x0f | 0x40
	u[8] = u[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", u[0:4], u[4:6], u[6:8], u[8:10], u[10:])
}

// doHTTP makes an HTTP client session's request and records the exchange.
func (r *debugRun) doHTTP(ctx context.Context, x debugSession, dial dialFunc) {
	target := clientTarget(x)
	if !strings.Contains(target, "://") {
		target = "http://" + target
	}
	method := strings.ToUpper(strings.TrimSpace(x.Method))
	if method == "" {
		method = http.MethodGet
	}
	ex := httpExchange{At: time.Now(), Method: method, URL: target}
	defer func() {
		r.mu.Lock()
		if len(r.exchanges) >= debugMaxExchanges {
			r.exchanges = append([]httpExchange(nil), r.exchanges[1:]...)
		}
		r.exchanges = append(r.exchanges, ex)
		r.state = debugIdle
		r.mu.Unlock()
		r.notify()
	}()
	r.setState(debugBusy)
	var body io.Reader
	if x.Body != "" && method != http.MethodGet && method != http.MethodHead {
		body = strings.NewReader(expandTemplate(x.Body, nil))
	}
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		ex.Err = err.Error()
		return
	}
	for _, h := range x.Headers {
		if name := strings.TrimSpace(h.Name); name != "" {
			req.Header.Add(name, expandTemplate(h.Value, nil))
		}
	}
	transport := &http.Transport{
		DialContext:     func(ctx context.Context, network, addr string) (net.Conn, error) { return dial(ctx, "tcp", addr) },
		TLSClientConfig: &tls.Config{InsecureSkipVerify: x.Insecure},
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 60 * time.Second,
		// Redirects are shown, not followed: the debugger shows what the
		// server said.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	started := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		ex.Err, ex.Duration = err.Error(), time.Since(started)
		return
	}
	defer resp.Body.Close()
	ex.Body, err = io.ReadAll(io.LimitReader(resp.Body, debugReadLimit))
	ex.Duration = time.Since(started)
	ex.Status, ex.Proto, ex.Headers = resp.StatusCode, resp.Proto, resp.Header
	if err != nil {
		ex.Err = err.Error()
	}
}

// directDial connects without the engine.
func directDial(ctx context.Context, network, address string) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, network, address)
}
