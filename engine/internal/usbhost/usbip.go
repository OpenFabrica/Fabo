// Package usbhost serves host USB devices over the guest's USB/IP transport.
// Device capture is platform-specific; framing and cancellation are shared.
package usbhost

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"
)

const maxTransfer = 16 << 20

type Setup struct {
	Type, Request        uint8
	Value, Index, Length uint16
}

func parseSetup(p []byte) Setup {
	return Setup{p[0], p[1], binary.LittleEndian.Uint16(p[2:]), binary.LittleEndian.Uint16(p[4:]), binary.LittleEndian.Uint16(p[6:])}
}

type Result struct {
	Status int32
	Data   []byte
	Actual int
}
type Backend interface {
	Control(uint32, Setup, []byte) Result
	Configure(uint8) int32
	Alternate(uint8, uint8) int32
	ClearHalt(uint8) int32
	Transfer(uint32, uint8, []byte, int) Result
	Abort(uint32, uint8)
	Close()
}
type urb struct {
	seq     uint32
	address uint8
	in      bool
	length  int
	setup   Setup
	data    []byte
}
type urbState struct {
	running, dropped, unlinking bool
	endpoint                    uint8
	unlink                      uint32
}
type endpointQueue struct {
	mu      sync.Mutex
	ready   *sync.Cond
	pending []urb
	closed  bool
}
type usbSession struct {
	backend Backend
	conn    net.Conn
	mu, out sync.Mutex
	states  map[uint32]*urbState
}

func (s *usbSession) send(command, seq uint32, status int32, actual int, data []byte) error {
	header := make([]byte, 48)
	binary.BigEndian.PutUint32(header, command)
	binary.BigEndian.PutUint32(header[4:], seq)
	binary.BigEndian.PutUint32(header[20:], uint32(status))
	if command == 3 {
		binary.BigEndian.PutUint32(header[24:], uint32(actual))
	}
	s.out.Lock()
	defer s.out.Unlock()
	_ = s.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	_, e := s.conn.Write(append(header, data...))
	if e != nil {
		_ = s.conn.Close()
	}
	return e
}
func (s *usbSession) dispatch(u urb) Result {
	if u.address != 0 {
		return s.backend.Transfer(u.seq, u.address, u.data, u.length)
	}
	switch {
	case u.setup.Type == 0 && u.setup.Request == 9:
		return Result{Status: s.backend.Configure(uint8(u.setup.Value))}
	case u.setup.Type == 1 && u.setup.Request == 11:
		return Result{Status: s.backend.Alternate(uint8(u.setup.Index), uint8(u.setup.Value))}
	case u.setup.Type == 2 && u.setup.Request == 1 && u.setup.Value == 0:
		return Result{Status: s.backend.ClearHalt(uint8(u.setup.Index))}
	}
	result := s.backend.Control(u.seq, u.setup, u.data)
	if u.in && len(result.Data) > int(u.setup.Length) {
		result.Data = result.Data[:u.setup.Length]
	}
	return result
}
func (s *usbSession) run(u urb) {
	s.mu.Lock()
	state := s.states[u.seq]
	if state == nil || state.dropped {
		delete(s.states, u.seq)
		s.mu.Unlock()
		return
	}
	state.running = true
	state.endpoint = u.address
	s.mu.Unlock()
	result := s.dispatch(u)
	if u.in {
		result.Data = result.Data[:min(len(result.Data), u.length)]
		result.Actual = len(result.Data)
	} else {
		result.Data = nil
	}
	s.mu.Lock()
	state = s.states[u.seq]
	delete(s.states, u.seq)
	s.mu.Unlock()
	if state != nil && state.unlinking {
		_ = s.send(4, state.unlink, -104, 0, nil)
	} else {
		_ = s.send(3, u.seq, result.Status, result.Actual, result.Data)
	}
}
func (s *usbSession) unlink(seq, target uint32) {
	s.mu.Lock()
	state := s.states[target]
	if state != nil && !state.dropped && !state.unlinking {
		if state.running {
			state.unlinking = true
			state.unlink = seq
			s.backend.Abort(target, state.endpoint)
			s.mu.Unlock()
			return
		}
		state.dropped = true
		s.mu.Unlock()
		_ = s.send(4, seq, -104, 0, nil)
		return
	}
	s.mu.Unlock()
	_ = s.send(4, seq, 0, 0, nil)
}

// Serve owns conn until return. On disconnect all blocked transfers are
// aborted before endpoint workers are joined; callers may then release hardware.
func Serve(backend Backend, conn net.Conn) error {
	s := &usbSession{backend: backend, conn: conn, states: map[uint32]*urbState{}}
	queues := map[uint8]*endpointQueue{}
	var workers sync.WaitGroup
	defer func() {
		conn.Close()
		backend.Close()
		for _, q := range queues {
			q.mu.Lock()
			q.closed = true
			q.pending = nil
			q.ready.Signal()
			q.mu.Unlock()
		}
		workers.Wait()
	}()
	for {
		var head [48]byte
		if _, e := io.ReadFull(conn, head[:]); e != nil {
			if errors.Is(e, io.EOF) {
				return nil
			}
			return e
		}
		get := func(n int) uint32 { return binary.BigEndian.Uint32(head[n:]) }
		command, seq := get(0), get(4)
		if command == 2 {
			s.unlink(seq, get(20))
			continue
		}
		if command != 1 {
			return fmt.Errorf("unknown USB/IP command %d", command)
		}
		dir, ep, length, packets := get(12), get(16), int32(get(24)), int32(get(32))
		if dir > 1 || ep > 15 || length < 0 || length > maxTransfer || packets < -1 || packets > 1024 {
			return errors.New("invalid USB/IP transfer header")
		}
		u := urb{seq: seq, in: dir == 1, address: uint8(ep), length: int(length), setup: parseSetup(head[40:])}
		if ep != 0 && u.in {
			u.address |= 0x80
		}
		if !u.in && length > 0 {
			u.data = make([]byte, length)
			if _, e := io.ReadFull(conn, u.data); e != nil {
				return e
			}
		}
		if packets > 0 {
			if _, e := io.CopyN(io.Discard, conn, int64(packets)*16); e != nil {
				return e
			}
			if e := s.send(3, seq, -22, 0, nil); e != nil {
				return e
			}
			continue
		}
		s.mu.Lock()
		if s.states[seq] != nil {
			s.mu.Unlock()
			return errors.New("duplicate USB/IP sequence")
		}
		s.states[seq] = &urbState{}
		s.mu.Unlock()
		q := queues[u.address]
		if q == nil {
			q = &endpointQueue{}
			q.ready = sync.NewCond(&q.mu)
			queues[u.address] = q
			workers.Add(1)
			go func(q *endpointQueue) {
				defer workers.Done()
				for {
					q.mu.Lock()
					for len(q.pending) == 0 && !q.closed {
						q.ready.Wait()
					}
					if q.closed {
						q.mu.Unlock()
						return
					}
					next := q.pending[0]
					q.pending[0] = urb{}
					q.pending = q.pending[1:]
					q.mu.Unlock()
					s.run(next)
				}
			}(q)
		}
		q.mu.Lock()
		q.pending = append(q.pending, u)
		q.ready.Signal()
		q.mu.Unlock()
	}
}
