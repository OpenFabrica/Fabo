// Package hostrelay lets Fabo's engine in WSL2 reach a proxy on Windows'
// own loopback, 127.0.0.1:7892 say, where most Windows proxy apps listen.
// In WSL's default NAT networking the distro's 127.0.0.1 is its own and
// Windows' firewall keeps the WSL network away from Windows' ports, so such
// a proxy cannot be dialled from the distro.
//
// The connections take the way fabo-win already talks to WSL: a wsl.exe
// process's stdin and stdout. fabo-win (Client, the Windows end) starts
// `fabo hostrelay` (ListenSide, the WSL end) and asks it to LISTEN for each
// proxy port; it listens on the distro's 127.0.0.1 and sends each
// connection back as frames, and fabo-win dials the proxy on Windows.
// fabo-win dials only the ports it asked for, so nothing in WSL can reach
// any other Windows port this way. The engine dials 127.0.0.1:<the port in
// WSL> for the Windows port (engine/internal/localengine/proxy.go).
//
// Frames are the bridge's (engine/internal/wslbridge): kind (1 byte),
// connection id (u32 BE), length (u32 BE), then the data. Kinds and
// payloads are byte-for-byte upstream Kiri's crates/kiri/src/hostrelay.rs;
// there is no ACK: each connection has a bounded queue instead.
//
// Differences from upstream, on purpose: both ends close their listeners
// and connections when the frame stream ends (upstream relied on the
// process exiting), and Client stops or restarts the relay when the proxy
// goes away or changes ports, where upstream kept every listener it ever
// opened.
package hostrelay

import (
	"bufio"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/OpenFabrica/Fabo/engine/internal/wslbridge"
)

// Frame kinds, as upstream's.
const (
	// KindOpen (WSL → Windows): a connection to the proxy on the port in
	// the data (u16 BE).
	KindOpen  byte = 1
	KindData  byte = 2
	KindEOF   byte = 3 // no more data from this side (a half-close)
	KindClose byte = 4 // the connection is gone, both ways
	// KindListen (Windows → WSL): listen for this Windows port (u16 BE).
	KindListen byte = 6
	// KindListening (WSL → Windows): the Windows port, then the distro's
	// port that leads to it (both u16 BE); 0 when it could not listen.
	KindListening byte = 7
)

const (
	// chunk is the most data either end puts in one frame.
	chunk = 64 << 10
	// queueLen is how many frames wait for one connection's writer. When
	// it is full the frame reader waits, which slows the other connections
	// too; a proxy relay has a few connections that all keep reading, and
	// nothing grows without bound (upstream's QUEUE).
	queueLen = 64
)

// sender writes whole frames from many goroutines onto one stream.
type sender struct {
	mu sync.Mutex
	w  *bufio.Writer
}

// newSender buffers w so a header and its data leave in one write.
func newSender(w io.Writer) *sender {
	return &sender{w: bufio.NewWriterSize(w, chunk+9)}
}

// send writes and flushes one frame.
func (s *sender) send(kind byte, id uint32, data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := wslbridge.WriteFrame(s.w, kind, id, data); err != nil {
		return err
	}
	return s.w.Flush()
}

// portAt reads a u16 BE at offset at, if the data holds one.
func portAt(data []byte, at int) (uint16, bool) {
	if len(data) < at+2 {
		return 0, false
	}
	return binary.BigEndian.Uint16(data[at:]), true
}

// portData encodes ports as consecutive u16 BE.
func portData(ports ...uint16) []byte {
	b := make([]byte, 0, 2*len(ports))
	for _, p := range ports {
		b = binary.BigEndian.AppendUint16(b, p)
	}
	return b
}

// op is one frame's worth of work for a connection's writer.
type op struct {
	data []byte
	eof  bool
}

// entry is one connection through the relay.
type entry struct {
	q chan op
	// gone is closed when the connection is forgotten (CLOSE, an error,
	// the end of the stream); stopped when its writer takes no more ops.
	// A Go channel send, unlike upstream's SyncSender, does not fail when
	// the receiver has gone: these keep dispatch from waiting forever.
	gone, stopped chan struct{}
	goneOnce      sync.Once
	stopOnce      sync.Once
	// conn is set once the connection is made; nil while it dials.
	conn net.Conn
}

// forget closes gone once.
func (e *entry) forget() { e.goneOnce.Do(func() { close(e.gone) }) }

// stop closes stopped once.
func (e *entry) stop() { e.stopOnce.Do(func() { close(e.stopped) }) }

// conns are the connections open through one relay, by id.
type conns struct {
	mu sync.Mutex
	m  map[uint32]*entry
}

// newConns returns an empty table.
func newConns() *conns { return &conns{m: map[uint32]*entry{}} }

// open registers a new connection: what arrives for it waits in its queue
// until it is made.
func (c *conns) open(id uint32) *entry {
	e := &entry{q: make(chan op, queueLen), gone: make(chan struct{}), stopped: make(chan struct{})}
	c.mu.Lock()
	if old := c.m[id]; old != nil {
		c.dropLocked(id, old)
	}
	c.m[id] = e
	c.mu.Unlock()
	return e
}

// remove forgets connection id if it is still e, closing its stream; it
// reports whether it did, so only one party says CLOSE.
func (c *conns) remove(id uint32, e *entry) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m[id] != e {
		return false
	}
	c.dropLocked(id, e)
	return true
}

// dropLocked forgets e and closes its stream.
func (c *conns) dropLocked(id uint32, e *entry) {
	delete(c.m, id)
	e.forget()
	if e.conn != nil {
		e.conn.Close()
	}
}

// closeAll forgets every connection: the frame stream has ended, and
// nothing on the other end will speak for them again.
func (c *conns) closeAll() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for id, e := range c.m {
		c.dropLocked(id, e)
	}
}

// dispatch hands a DATA, EOF or CLOSE frame to its connection.
func (c *conns) dispatch(kind byte, id uint32, data []byte) {
	var o op
	switch kind {
	case KindData:
		o.data = data
	case KindEOF:
		o.eof = true
	case KindClose:
		c.mu.Lock()
		if e := c.m[id]; e != nil {
			c.dropLocked(id, e)
		}
		c.mu.Unlock()
		return
	default:
		return
	}
	c.mu.Lock()
	e := c.m[id]
	c.mu.Unlock()
	if e == nil {
		return
	}
	// Sent outside the lock: a full queue waits for its own writer only.
	select {
	case e.q <- o:
	case <-e.gone:
	case <-e.stopped:
	}
}

// halfCloser is a stream that can say it will write no more.
type halfCloser interface{ CloseWrite() error }

// attach joins stream to connection id: what it reads goes to the other
// end as DATA, then EOF; what arrives for it (e's queue) is written to it.
func (c *conns) attach(s *sender, id uint32, e *entry, stream net.Conn) {
	if tcp, ok := stream.(*net.TCPConn); ok {
		_ = tcp.SetNoDelay(true)
	}
	c.mu.Lock()
	if c.m[id] != e {
		// Closed while it was being made.
		c.mu.Unlock()
		stream.Close()
		return
	}
	e.conn = stream
	c.mu.Unlock()
	// Each half that ends counts; the second forgets the connection.
	var halves atomic.Int32
	done := func() {
		if halves.Add(1) == 2 {
			c.remove(id, e)
		}
	}
	go func() {
		defer done()
		defer e.stop()
		for {
			select {
			case <-e.gone:
				return
			case o := <-e.q:
				if o.eof {
					if hc, ok := stream.(halfCloser); ok {
						_ = hc.CloseWrite()
					} else {
						stream.Close()
					}
					return
				}
				if _, err := stream.Write(o.data); err != nil {
					if c.remove(id, e) {
						_ = s.send(KindClose, id, nil)
					}
					return
				}
			}
		}
	}()
	go func() {
		defer done()
		buf := make([]byte, chunk)
		for {
			n, err := stream.Read(buf)
			if n > 0 {
				if s.send(KindData, id, buf[:n]) != nil {
					return
				}
			}
			if errors.Is(err, io.EOF) {
				_ = s.send(KindEOF, id, nil)
				return
			}
			if err != nil {
				// Gone while the other end may still be sending: say so.
				if c.remove(id, e) {
					_ = s.send(KindClose, id, nil)
				}
				return
			}
		}
	}()
}

// ListenSide is the WSL end (`fabo hostrelay`): it listens on the distro's
// loopback as the Windows end asks and sends each connection over out,
// until in ends (fabo-win has gone), when it closes its listeners and
// connections.
func ListenSide(in io.Reader, out io.Writer) error {
	s := newSender(out)
	cs := newConns()
	var next atomic.Uint32
	listening := map[uint16]uint16{}
	var listeners []net.Listener
	defer func() {
		for _, l := range listeners {
			l.Close()
		}
		cs.closeAll()
	}()
	r := bufio.NewReaderSize(in, chunk)
	for {
		f, err := wslbridge.ReadFrame(r)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if f.Kind != KindListen {
			cs.dispatch(f.Kind, f.ID, f.Data)
			continue
		}
		target, ok := portAt(f.Data, 0)
		if !ok {
			continue
		}
		local, ok := listening[target]
		if !ok {
			if l, err := net.Listen("tcp4", "127.0.0.1:0"); err == nil {
				listeners = append(listeners, l)
				local = uint16(l.Addr().(*net.TCPAddr).Port)
				listening[target] = local
				go accept(l, target, cs, s, &next)
			}
		}
		if err := s.send(KindListening, 0, portData(target, local)); err != nil {
			return err
		}
	}
}

// accept sends each connection to l to the Windows end, for target.
func accept(l net.Listener, target uint16, cs *conns, s *sender, next *atomic.Uint32) {
	for {
		c, err := l.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			time.Sleep(50 * time.Millisecond)
			continue
		}
		id := next.Add(1)
		e := cs.open(id)
		if s.send(KindOpen, id, portData(target)) != nil {
			cs.remove(id, e)
			c.Close()
			return
		}
		cs.attach(s, id, e, c)
	}
}

// DialSide is the Windows end over in and out: it dials (with dial) each
// connection the WSL end sends, on the ports allowed names, and hands
// LISTENING replies to listening. It returns when in ends, closing the
// connections it made.
func DialSide(in io.Reader, out io.Writer, allowed func(uint16) bool, listening func(target, local uint16), dial func(uint16) (net.Conn, error)) error {
	return dialSide(in, newSender(out), allowed, listening, dial)
}

// dialSide is DialSide with a sender the caller also writes LISTEN on.
func dialSide(in io.Reader, s *sender, allowed func(uint16) bool, listening func(target, local uint16), dial func(uint16) (net.Conn, error)) error {
	cs := newConns()
	defer cs.closeAll()
	r := bufio.NewReaderSize(in, chunk)
	for {
		f, err := wslbridge.ReadFrame(r)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		switch f.Kind {
		case KindOpen:
			target, ok := portAt(f.Data, 0)
			if !ok || !allowed(target) {
				if err := s.send(KindClose, f.ID, nil); err != nil {
					return err
				}
				continue
			}
			// Data that arrives while it dials waits in the queue.
			e := cs.open(f.ID)
			go func(id uint32) {
				c, err := dial(target)
				if err != nil {
					if cs.remove(id, e) {
						_ = s.send(KindClose, id, nil)
					}
					return
				}
				cs.attach(s, id, e, c)
			}(f.ID)
		case KindListening:
			target, ok1 := portAt(f.Data, 0)
			local, ok2 := portAt(f.Data, 2)
			if ok1 && ok2 {
				listening(target, local)
			}
		default:
			cs.dispatch(f.Kind, f.ID, f.Data)
		}
	}
}

// DialLoopback dials this machine's loopback at port: IPv4, then IPv6.
func DialLoopback(port uint16) (net.Conn, error) {
	d := net.Dialer{Timeout: 5 * time.Second}
	p := strconv.Itoa(int(port))
	c, err := d.Dial("tcp4", net.JoinHostPort("127.0.0.1", p))
	if err == nil {
		return c, nil
	}
	if c6, err6 := d.Dial("tcp6", net.JoinHostPort("::1", p)); err6 == nil {
		return c6, nil
	}
	return nil, err
}
