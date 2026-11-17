package wslbridge

import (
	"sync"
	"time"
)

// credit is one connection's credit one way: bytes sent and not yet
// acknowledged by the other side.
type credit struct {
	mu       sync.Mutex
	inflight int
	closed   bool
	changed  chan struct{} // closed and replaced on every ack or close
}

// newCredit returns an open credit with nothing in flight.
func newCredit() *credit { return &credit{changed: make(chan struct{})} }

// take waits while a whole Window is unacknowledged, then counts n more.
// It returns false once the connection has closed, or when alive (checked
// every half second while waiting) says the local peer has gone.
func (c *credit) take(n int, alive func() bool) bool {
	c.mu.Lock()
	for c.inflight >= Window && !c.closed {
		changed := c.changed
		c.mu.Unlock()
		select {
		case <-changed:
		case <-time.After(500 * time.Millisecond):
			if alive != nil && !alive() {
				return false
			}
		}
		c.mu.Lock()
	}
	defer c.mu.Unlock()
	if c.closed {
		return false
	}
	c.inflight += n
	return true
}

// ack returns n bytes of credit.
func (c *credit) ack(n int) {
	c.mu.Lock()
	c.inflight = max(0, c.inflight-n)
	c.wake()
	c.mu.Unlock()
}

// close ends every current and future take.
func (c *credit) close() {
	c.mu.Lock()
	if !c.closed {
		c.closed = true
		c.wake()
	}
	c.mu.Unlock()
}

// wake releases waiters; c.mu is held.
func (c *credit) wake() {
	close(c.changed)
	c.changed = make(chan struct{})
}

// op is one step for a connection's writer, in order.
type op struct {
	kind byte // KindData, KindEOF or KindClose
	data []byte
}

// queue is a connection's unbounded op queue. What bounds it is the byte
// count its producer checks against the credit window, never a blocking
// push: the producer serves every connection and must not wait on one.
type queue struct {
	mu     sync.Mutex
	items  []op
	bytes  int
	closed bool
	ready  chan struct{} // one pending signal
}

// newQueue returns an empty open queue.
func newQueue() *queue { return &queue{ready: make(chan struct{}, 1)} }

// push appends o unless the queue was closed. bound (when positive)
// refuses data that would take the queued bytes past it, and reports
// overflow instead.
func (q *queue) push(o op, bound int) (overflow bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return false
	}
	if bound > 0 && q.bytes+len(o.data) > bound {
		return true
	}
	q.items = append(q.items, o)
	q.bytes += len(o.data)
	q.signal()
	return false
}

// pop waits for the next op; ok is false once the queue is closed and empty.
func (q *queue) pop() (op, bool) {
	for {
		q.mu.Lock()
		if len(q.items) > 0 {
			o := q.items[0]
			q.items[0] = op{}
			q.items = q.items[1:]
			q.mu.Unlock()
			return o, true
		}
		if q.closed {
			q.mu.Unlock()
			return op{}, false
		}
		q.mu.Unlock()
		<-q.ready
	}
}

// done subtracts n handed-on bytes from the queue's count.
func (q *queue) done(n int) {
	q.mu.Lock()
	q.bytes -= n
	q.mu.Unlock()
}

// close lets pop drain what is queued and then report the end.
func (q *queue) close() {
	q.mu.Lock()
	q.closed = true
	q.signal()
	q.mu.Unlock()
}

// signal wakes a waiting pop; q.mu is held.
func (q *queue) signal() {
	select {
	case q.ready <- struct{}{}:
	default:
	}
}
