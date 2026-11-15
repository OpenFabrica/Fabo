package sharewire

import (
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
)

// Link is the session a Stream sends through. A session has one writer
// goroutine fed by two queues: control frames go first so Window, Pong and
// Reset never wait behind a slow stream's data.
type Link interface {
	// SendCtrl queues a control frame without blocking; false means the
	// session is over (a full queue ends it).
	SendCtrl(Msg) bool
	// SendData queues a Data or Close frame, waiting for room; false
	// means the session is over. Close goes here to stay behind the data.
	SendData(Msg) bool
}

// Stream is one multiplexed connection: the peer's bytes are queued and
// written to a local net.Conn, and the local conn's bytes are sent to the
// peer within the credit the peer grants.
//
// Flow control, per direction:
//   - Sending: credit starts at sendWindow and never exceeds it; each Data
//     carries at most min(credit, MaxChunk) bytes.
//   - Receiving: the peer may have recvWindow bytes unacknowledged, which
//     is also the queue's capacity; credit is returned as bytes are
//     written locally, at recvThreshold or when the queue drains.
//
// Close is a half-close: the peer's Close shuts the local write side once
// the queue drains, while local bytes keep flowing until local EOF.
type Stream struct {
	ID         uint32
	link       Link
	sendWindow uint32
	recvWindow uint32
	// OnReset runs once when the stream is reset locally, before Reset is
	// sent, so the session can forget it.
	OnReset func()

	mu   sync.Mutex
	cond *sync.Cond
	// queue holds peer→local bytes, at most recvWindow of them.
	queue  [][]byte
	queued int
	eof    bool
	// credit is what may still be sent to the peer.
	credit uint32
	dead   bool
	conn   net.Conn
	// firstIn and firstOut are the first chunks in each direction, kept for
	// request and status sniffing.
	firstIn, firstOut []byte

	// In counts peer→local bytes and Out local→peer bytes.
	In, Out atomic.Uint64
}

// NewStream returns a stream that may send sendWindow bytes before credit
// and accepts recvWindow bytes unacknowledged.
func NewStream(id uint32, link Link, sendWindow, recvWindow uint32) *Stream {
	st := &Stream{ID: id, link: link, sendWindow: sendWindow, recvWindow: recvWindow, credit: sendWindow}
	st.cond = sync.NewCond(&st.mu)
	return st
}

// recvThreshold returns credit at a quarter of the window, at most 64KiB.
func (st *Stream) recvThreshold() int {
	return max(1, min(64*1024, int(st.recvWindow)/4))
}

// Push queues peer→local bytes (copied); false when they exceed the
// window, which means the peer broke flow control.
func (st *Stream) Push(data []byte) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.dead || st.eof {
		return true
	}
	if st.queued+len(data) > int(st.recvWindow) {
		return false
	}
	if len(data) == 0 {
		return true
	}
	st.queue = append(st.queue, append([]byte(nil), data...))
	st.queued += len(data)
	st.In.Add(uint64(len(data)))
	st.cond.Broadcast()
	return true
}

// PushEOF records the peer's half-close.
func (st *Stream) PushEOF() {
	st.mu.Lock()
	st.eof = true
	st.cond.Broadcast()
	st.mu.Unlock()
}

// Grant adds send credit; false when it is zero or overflows the window.
func (st *Stream) Grant(credit uint32) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	if credit == 0 || uint64(st.credit)+uint64(credit) > uint64(st.sendWindow) {
		return false
	}
	st.credit += credit
	st.cond.Broadcast()
	return true
}

// Kill drops the stream locally without telling the peer: it closes the
// local conn and wakes the pumps.
func (st *Stream) Kill() {
	st.mu.Lock()
	st.dead = true
	conn := st.conn
	st.cond.Broadcast()
	st.mu.Unlock()
	if conn != nil {
		conn.Close()
	}
}

// Reset drops the stream and tells the peer, once.
func (st *Stream) Reset() {
	st.mu.Lock()
	already := st.dead
	st.mu.Unlock()
	if already {
		return
	}
	st.Kill()
	if st.OnReset != nil {
		st.OnReset()
	}
	st.link.SendCtrl(Reset{Stream: st.ID})
}

// Dead reports whether the stream was killed or reset.
func (st *Stream) Dead() bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.dead
}

// FirstIn returns the first peer→local chunk, if any arrived yet.
func (st *Stream) FirstIn() []byte {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.firstIn != nil {
		return st.firstIn
	}
	if len(st.queue) > 0 {
		return st.queue[0]
	}
	return nil
}

// FirstOut returns the first local→peer chunk, if any was sent.
func (st *Stream) FirstOut() []byte {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.firstOut
}

// Pump attaches conn and carries both directions until both have ended or
// the stream dies; it closes conn before returning.
func (st *Stream) Pump(conn net.Conn) {
	st.mu.Lock()
	if st.dead {
		st.mu.Unlock()
		conn.Close()
		return
	}
	st.conn = conn
	st.mu.Unlock()
	done := make(chan struct{})
	go func() {
		defer close(done)
		st.toLocal(conn)
	}()
	st.fromLocal(conn)
	<-done
	conn.Close()
}

// toLocal writes queued peer bytes in order, returning credit as they are
// written; after the peer's Close it half-closes conn.
func (st *Stream) toLocal(conn net.Conn) {
	threshold := st.recvThreshold()
	unacked := 0
	for {
		st.mu.Lock()
		for len(st.queue) == 0 && !st.eof && !st.dead {
			st.cond.Wait()
		}
		if st.dead {
			st.mu.Unlock()
			return
		}
		if len(st.queue) == 0 {
			st.mu.Unlock()
			if unacked > 0 {
				st.link.SendCtrl(Window{Stream: st.ID, Credit: uint32(unacked)})
			}
			closeWrite(conn)
			return
		}
		chunk := st.queue[0]
		st.queue[0] = nil
		st.queue = st.queue[1:]
		st.queued -= len(chunk)
		drained := len(st.queue) == 0
		if st.firstIn == nil {
			st.firstIn = chunk
		}
		st.mu.Unlock()
		if _, err := conn.Write(chunk); err != nil {
			st.Reset()
			return
		}
		unacked += len(chunk)
		// Return credit at the threshold, and whatever is left once the
		// queue drains, so a small window never stalls the sender.
		if unacked >= threshold || drained {
			if !st.link.SendCtrl(Window{Stream: st.ID, Credit: uint32(unacked)}) {
				return
			}
			unacked = 0
		}
	}
}

// fromLocal sends local bytes within the granted credit; at local EOF it
// sends Close through the data queue so it follows the last Data.
func (st *Stream) fromLocal(conn net.Conn) {
	buf := make([]byte, MaxChunk)
	for {
		st.mu.Lock()
		for st.credit == 0 && !st.dead {
			st.cond.Wait()
		}
		if st.dead {
			st.mu.Unlock()
			return
		}
		allow := min(int(st.credit), MaxChunk)
		st.mu.Unlock()
		n, err := conn.Read(buf[:allow])
		if n > 0 {
			st.mu.Lock()
			if st.firstOut == nil {
				st.firstOut = append([]byte(nil), buf[:n]...)
			}
			dead := st.dead
			st.credit -= uint32(n)
			st.mu.Unlock()
			if dead || !st.link.SendData(Data{Stream: st.ID, Data: buf[:n]}) {
				return
			}
			st.Out.Add(uint64(n))
		}
		if err != nil {
			if st.Dead() {
				return
			}
			if errors.Is(err, io.EOF) {
				st.link.SendData(Close{Stream: st.ID})
			} else {
				st.Reset()
			}
			return
		}
	}
}

// closeWrite half-closes conn when it supports it (TCP, or TLS which sends
// close_notify).
func closeWrite(conn net.Conn) {
	if cw, ok := conn.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
	}
}
