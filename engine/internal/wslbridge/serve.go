package wslbridge

import (
	"errors"
	"fmt"
	"io"
	"sync"
)

// Conn is a stream with half-closes, as a Unix socket or a message-mode
// named pipe connection is.
type Conn interface {
	io.ReadWriteCloser
	CloseWrite() error
}

// readCloser is implemented by connections that can stop reading without
// closing (net.UnixConn, net.TCPConn).
type readCloser interface{ CloseRead() error }

// serveConn is one connection on the WSL side.
type serveConn struct {
	id uint32
	d  Conn
	q  *queue
	// out is the credit for the daemon's replies when the Windows side
	// asked for flow control; nil otherwise.
	out *credit

	mu sync.Mutex
	// hostEOF: the writer has passed on the host's half-close, so no more
	// input can come. daemonDone: the reply reader has finished. With both,
	// the connection is over even though the host sends no CLOSE after an
	// EOF; upstream Kiri kept such connections until the bridge ended.
	hostEOF, daemonDone bool
}

// server is the WSL end's state.
type server struct {
	w     *frameWriter
	mu    sync.Mutex
	conns map[uint32]*serveConn
}

// Serve is the WSL end, `fabo bridge`: frames from the Windows host arrive
// on in, each connection is relayed to a fresh connection from dial (the
// daemon's Unix socket), and the replies go back on out. It returns when in
// ends (the host has gone, and its connections with it).
func Serve(in io.Reader, out io.Writer, dial func() (Conn, error)) error {
	return newServer(out).run(in, dial)
}

// newServer returns the WSL end writing frames to out.
func newServer(out io.Writer) *server {
	return &server{w: newFrameWriter(out), conns: map[uint32]*serveConn{}}
}

// run is Serve's frame loop.
func (s *server) run(in io.Reader, dial func() (Conn, error)) error {
	// Hello: this bridge does flow control.
	if err := s.w.send(KindAck, 0, nil); err != nil {
		return err
	}
	defer func() {
		s.mu.Lock()
		for id, c := range s.conns {
			s.end(c)
			delete(s.conns, id)
		}
		s.mu.Unlock()
	}()
	for {
		f, err := ReadFrame(in)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		switch f.Kind {
		case KindOpen:
			s.mu.Lock()
			if old := s.conns[f.ID]; old != nil {
				// A reused id replaces a connection the host forgot.
				s.end(old)
				delete(s.conns, f.ID)
			}
			s.mu.Unlock()
			d, err := dial()
			if err != nil {
				if err := s.w.send(KindClose, f.ID, nil); err != nil {
					return err
				}
				continue
			}
			c := &serveConn{id: f.ID, d: d, q: newQueue()}
			flow := len(f.Data) > 0 && f.Data[0] == 1
			if flow {
				c.out = newCredit()
			}
			s.mu.Lock()
			s.conns[f.ID] = c
			s.mu.Unlock()
			// The reader closes d once the writer has stopped using it.
			var writing sync.WaitGroup
			writing.Add(1)
			go func() {
				defer writing.Done()
				s.writer(c, flow)
			}()
			go s.replies(c, &writing)
		case KindAck:
			s.mu.Lock()
			c := s.conns[f.ID]
			s.mu.Unlock()
			if n, ok := ackCount(f.Data); ok && c != nil && c.out != nil {
				c.out.ack(n)
			}
		case KindData, KindEOF, KindClose:
			s.mu.Lock()
			c := s.conns[f.ID]
			s.mu.Unlock()
			if c == nil {
				continue
			}
			// Past the window: the host sent beyond its credit, or (without
			// credit) the daemon is not taking this connection's input.
			overflow := c.q.push(op{kind: f.Kind, data: f.Data}, limit)
			if overflow || f.Kind == KindClose {
				s.forget(c)
				s.mu.Lock()
				s.end(c)
				s.mu.Unlock()
				if overflow {
					if err := s.w.send(KindClose, f.ID, nil); err != nil {
						return err
					}
				}
			}
		default:
			return fmt.Errorf("wslbridge: bad frame kind %d", f.Kind)
		}
	}
}

// end stops a connection both ways: its writer finishes what is queued
// and returns, and a write blocked on a daemon that stopped reading fails.
func (s *server) end(c *serveConn) {
	c.q.close()
	if c.out != nil {
		c.out.close()
	}
	shutdown(c.d)
}

// forget drops c from the table if it is still the connection under its id.
func (s *server) forget(c *serveConn) {
	s.mu.Lock()
	if s.conns[c.id] == c {
		delete(s.conns, c.id)
	}
	s.mu.Unlock()
}

// writer writes one connection's input to the daemon, acknowledging it as
// it goes when the host does flow control. Once the daemon's side has
// gone, input is still taken and acknowledged, unwritten, so the host's
// sender never waits for credit that cannot come.
func (s *server) writer(c *serveConn, flow bool) {
	broken := false
	pending := 0
	for {
		o, ok := c.q.pop()
		if !ok {
			return
		}
		switch o.kind {
		case KindData:
			if !broken {
				if _, err := c.d.Write(o.data); err != nil {
					broken = true
					shutdown(c.d)
				}
			}
			c.q.done(len(o.data))
			pending += len(o.data)
			if flow && (pending >= AckEvery || broken) {
				if s.w.send(KindAck, c.id, ackData(pending)) != nil {
					return
				}
				pending = 0
			}
		case KindEOF:
			_ = c.d.CloseWrite()
			c.mu.Lock()
			c.hostEOF = true
			over := c.daemonDone
			c.mu.Unlock()
			if over {
				s.forget(c)
				return
			}
		case KindClose:
			return
		}
	}
}

// shutdown ends both directions of d without releasing it, so its reader
// sees EOF; a connection that cannot stop reading is closed outright.
func shutdown(d Conn) {
	_ = d.CloseWrite()
	if r, ok := d.(readCloser); ok {
		_ = r.CloseRead()
		return
	}
	_ = d.Close()
}

// replies copies the daemon's side of one connection back to the host,
// ending with EOF (or CLOSE on an error). With credit, it waits for the
// host's acknowledgements before each piece.
func (s *server) replies(c *serveConn, writing *sync.WaitGroup) {
	defer func() {
		c.mu.Lock()
		c.daemonDone = true
		over := c.hostEOF
		c.mu.Unlock()
		if over {
			// The writer has passed on the host's EOF and waits for
			// nothing but a CLOSE that will not come: release it.
			s.forget(c)
			c.q.close()
		}
		writing.Wait()
		_ = c.d.Close()
	}()
	buf := make([]byte, chunkSize)
	end := KindEOF
	for {
		n, err := c.d.Read(buf)
		if n > 0 {
			// The host closed the connection while this waited.
			if c.out != nil && !c.out.take(n, nil) {
				return
			}
			if s.w.send(KindData, c.id, buf[:n]) != nil {
				return
			}
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				end = KindClose
			}
			break
		}
	}
	_ = s.w.send(end, c.id, nil)
}
