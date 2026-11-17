package wslbridge

import (
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// ErrNotSetUp is what a Mux's Start returns while there is no distro to
// bridge to yet; such connections are refused quietly.
var ErrNotSetUp = errors.New("fabo is not set up yet")

// helloWait is how long a new connection waits for a bridge that has just
// started to say it does flow control; an older one never does.
const helloWait = 5 * time.Second

// lingerFor bounds the wait for a client to close its end after both
// half-closes (see linger).
const lingerFor = 2 * time.Second

// Transport is one running bridge process: its stdin, its stdout, and a
// wait for it to exit once its stdout has ended.
type Transport struct {
	In   io.WriteCloser
	Out  io.Reader
	Wait func() error
}

// Mux is the Windows end: it carries client connections over one bridge
// at a time, starting a new one when the last has died (WSL shut down,
// the distro restarted).
type Mux struct {
	// Start runs a new bridge. ErrNotSetUp means there is nothing to
	// bridge to yet.
	Start func() (*Transport, error)
	// Refuse, when set, may answer a client that got nothing from the
	// engine (setup unfinished); it reports whether it did.
	Refuse func(Conn) bool
	// Logf records what happens; nil discards it.
	Logf func(format string, args ...any)

	mu      sync.Mutex
	current *bridge
	nextID  atomic.Uint32
}

// logf logs when the Mux has a logger.
func (m *Mux) logf(format string, args ...any) {
	if m.Logf != nil {
		m.Logf(format, args...)
	}
}

// Reset leaves the current bridge to end and makes the next connection
// start a new one: setup has just changed the distro.
func (m *Mux) Reset() {
	m.mu.Lock()
	b := m.current
	m.current = nil
	m.mu.Unlock()
	if b != nil {
		b.kill()
	}
}

// Warm starts a bridge now, so the first docker command does not wait for
// WSL to boot.
func (m *Mux) Warm() error {
	_, err := m.bridge()
	return err
}

// bridge returns the live bridge, starting one if needed.
func (m *Mux) bridge() (*bridge, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.current != nil && !m.current.dead.Load() {
		return m.current, nil
	}
	t, err := m.Start()
	if err != nil {
		return nil, err
	}
	b := &bridge{
		w: newFrameWriter(t.In), in: t.In, conns: map[uint32]*muxConn{},
		hello: make(chan struct{}), gone: make(chan struct{}), started: time.Now(),
	}
	m.current = b
	m.logf("bridge started")
	go b.replies(m, t)
	return b, nil
}

// muxConn is one client connection, as the bridge's reply reader sees it.
type muxConn struct {
	q *queue
	// in is the credit for the client's input when the bridge does flow
	// control; nil with an older bridge.
	in *credit
}

// close ends the connection's queue and credit.
func (c *muxConn) close() {
	c.q.close()
	if c.in != nil {
		c.in.close()
	}
}

// bridge is one running bridge process.
type bridge struct {
	w     *frameWriter
	in    io.Closer
	mu    sync.Mutex
	conns map[uint32]*muxConn
	dead  atomic.Bool
	// credit: the bridge said hello, so it does flow control.
	credit    atomic.Bool
	hello     chan struct{}
	gone      chan struct{}
	helloOnce sync.Once
	goneOnce  sync.Once
	started   time.Time
}

// kill marks the bridge dead and closes its stdin, which ends the process.
func (b *bridge) kill() {
	b.dead.Store(true)
	_ = b.in.Close()
}

// send writes a frame to the bridge; a failure marks it dead.
func (b *bridge) send(kind byte, id uint32, data []byte) error {
	err := b.w.send(kind, id, data)
	if err != nil {
		b.dead.Store(true)
	}
	return err
}

// waitCredit reports whether the bridge does flow control, waiting for its
// hello while it has only just started.
func (b *bridge) waitCredit() bool {
	if b.credit.Load() {
		return true
	}
	left := helloWait - time.Since(b.started)
	if left <= 0 {
		return false
	}
	t := time.NewTimer(left)
	defer t.Stop()
	select {
	case <-b.hello:
		return true
	case <-b.gone:
	case <-t.C:
	}
	return b.credit.Load()
}

// replies hands each reply to its connection until the bridge exits. It
// never blocks on one connection: with credit the bridge never sends past
// the window; without it, a client that stopped reading is dropped.
func (b *bridge) replies(m *Mux, t *Transport) {
	for {
		f, err := ReadFrame(t.Out)
		if err != nil {
			break
		}
		if f.Kind == KindAck {
			if f.ID == 0 {
				b.credit.Store(true)
				b.helloOnce.Do(func() { close(b.hello) })
				continue
			}
			b.mu.Lock()
			c := b.conns[f.ID]
			b.mu.Unlock()
			if n, ok := ackCount(f.Data); ok && c != nil && c.in != nil {
				c.in.ack(n)
			}
			continue
		}
		kind := f.Kind
		if kind != KindData && kind != KindEOF {
			kind = KindClose
		}
		b.mu.Lock()
		c := b.conns[f.ID]
		if c == nil {
			b.mu.Unlock()
			continue
		}
		bound := limitNoCredit
		if c.in != nil {
			bound = limit
		}
		if c.q.push(op{kind: kind, data: f.Data}, bound) {
			delete(b.conns, f.ID)
			b.mu.Unlock()
			m.logf("connection %d: client stopped reading; closing it", f.ID)
			c.close()
			_ = b.send(KindClose, f.ID, nil)
			continue
		}
		b.mu.Unlock()
	}
	b.dead.Store(true)
	b.goneOnce.Do(func() { close(b.gone) })
	// Every connection still open ends now. Registration checks dead under
	// the same lock, so none can slip in after this sweep.
	b.mu.Lock()
	for id, c := range b.conns {
		c.close()
		delete(b.conns, id)
	}
	b.mu.Unlock()
	_ = b.in.Close()
	var status error
	if t.Wait != nil {
		status = t.Wait()
	}
	m.logf("bridge exited: %v", status)
}

// Serve carries one client connection over the bridge and returns when it
// has ended both ways. It closes c.
func (m *Mux) Serve(c Conn) {
	defer c.Close()
	alive := aliveFunc(c)
	var (
		b  *bridge
		id uint32
		mc *muxConn
	)
	// A bridge can die between bridge() and registering the connection
	// (while WSL boots, wsl.exe may exit at once). The registration checks
	// dead under the lock the dying bridge's sweep takes, and a bridge
	// found dead is left for the next one.
	for tries := 0; ; tries++ {
		var err error
		b, err = m.bridge()
		if err != nil {
			if !errors.Is(err, ErrNotSetUp) {
				m.logf("bridge: %v", err)
			}
			if m.Refuse != nil && m.Refuse(c) {
				linger(alive)
			}
			return
		}
		id = m.nextID.Add(1)
		mc = &muxConn{q: newQueue()}
		if b.waitCredit() {
			mc.in = newCredit()
		}
		b.mu.Lock()
		registered := !b.dead.Load()
		if registered {
			b.conns[id] = mc
		}
		b.mu.Unlock()
		open := []byte(nil)
		if mc.in != nil {
			open = []byte{1}
		}
		if registered && b.send(KindOpen, id, open) == nil {
			break
		}
		b.mu.Lock()
		delete(b.conns, id)
		b.mu.Unlock()
		if tries == 2 {
			m.logf("bridge: died three times while a connection opened; giving up on it")
			return
		}
	}
	replied := make(chan bool, 1)
	go func() { replied <- m.toClient(c, b, id, mc) }()
	buf := make([]byte, chunkSize)
	end := KindEOF
	for {
		n, err := c.Read(buf)
		if n > 0 {
			// Out of credit with the client gone, or the connection closed.
			if mc.in != nil && !mc.in.take(n, alive) {
				end = KindClose
				break
			}
			if b.send(KindData, id, buf[:n]) != nil {
				end = KindClose
				break
			}
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				end = KindClose
			}
			break
		}
	}
	_ = b.send(end, id, nil)
	if end == KindClose {
		// Nothing more will be read: stop the reply writer too.
		mc.close()
	}
	if <-replied && end == KindEOF {
		linger(alive)
	}
	b.mu.Lock()
	if b.conns[id] == mc {
		delete(b.conns, id)
	}
	b.mu.Unlock()
	mc.close()
}

// toClient writes the bridge's replies to the client, acknowledging them
// when the bridge does flow control. It reports whether they ended with
// EOF.
func (m *Mux) toClient(c Conn, b *bridge, id uint32, mc *muxConn) bool {
	pending := 0
	answered := false
	for {
		o, ok := mc.q.pop()
		if !ok {
			break
		}
		switch o.kind {
		case KindData:
			if _, err := c.Write(o.data); err != nil {
				mc.close()
				_ = c.Close()
				return false
			}
			answered = true
			mc.q.done(len(o.data))
			pending += len(o.data)
			if mc.in != nil && pending >= AckEvery {
				_ = b.send(KindAck, id, ackData(pending))
				pending = 0
			}
		case KindEOF:
			return c.CloseWrite() == nil
		case KindClose:
			// Closed before a word with setup unfinished: no engine is
			// listening in the distro yet. Say why.
			if !answered && m.Refuse != nil && m.Refuse(c) {
				return true
			}
			mc.close()
			_ = c.Close()
			return false
		}
	}
	// The bridge died with the connection open (the engine restarting):
	// end the client's side too, or a client waiting for its reply waits
	// forever. Closing c also ends the reader blocked on it.
	mc.close()
	_ = c.Close()
	return false
}

// aliveFunc returns c's liveness check when it has one (a named pipe can
// tell whether its client is still connected without reading).
func aliveFunc(c Conn) func() bool {
	if a, ok := c.(interface{ Alive() bool }); ok {
		return a.Alive
	}
	return nil
}

// linger waits a while for a client that has had both half-closes to
// close its end. Disconnecting a named pipe instance discards what the
// client has not read yet, so a client that had not yet read the empty
// message that is its EOF would read "No process is on the other end of
// the pipe" instead. Without a liveness check there is nothing to wait for.
func linger(alive func() bool) {
	if alive == nil {
		return
	}
	until := time.Now().Add(lingerFor)
	for alive() && time.Now().Before(until) {
		time.Sleep(2 * time.Millisecond)
	}
}

// RefuseHTTP answers c with a 503 and message, as the Docker API would, so
// docker prints "Error response from daemon: <message>"; it then half-closes.
func RefuseHTTP(c Conn, message string) bool {
	body, _ := json.Marshal(map[string]string{"message": message})
	reply := "HTTP/1.1 503 Service Unavailable\r\nContent-Type: application/json\r\nContent-Length: " +
		strconv.Itoa(len(body)) + "\r\nConnection: close\r\n\r\n" + string(body)
	if _, err := io.WriteString(c, reply); err != nil {
		return false
	}
	_ = c.CloseWrite()
	return true
}
