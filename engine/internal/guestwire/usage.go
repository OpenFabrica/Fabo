package guestwire

import (
	"context"
	"fmt"
)

const (
	UsageQueryMessage Kind = 74
	UsageMessage      Kind = 75
)

type usageReply struct {
	lines []string
	err   error
}

// Usage uses the original Kiri usage frames and a separate reply slot, so
// disk traversal does not reserve the request gate used by lifecycle calls.
func (s *Session) Usage(ctx context.Context) ([]string, error) {
	reply := make(chan usageReply, 1)
	s.mu.Lock()
	if s.usage != nil {
		s.mu.Unlock()
		return nil, fmt.Errorf("guest disk measurement already in progress")
	}
	s.usage = reply
	s.mu.Unlock()
	if err := s.send(ctx, UsageQueryMessage, encoder{}); err != nil {
		s.mu.Lock()
		if s.usage == reply {
			s.usage = nil
		}
		s.mu.Unlock()
		return nil, err
	}
	select {
	case r := <-reply:
		return r.lines, r.err
	case <-s.done:
		return nil, s.Err()
	case <-ctx.Done():
		// Keep the slot reserved until the guest replies. The wire message
		// has no request ID and must not complete a later request.
		return nil, ctx.Err()
	}
}

func (s *Session) acceptUsage(frame Frame) {
	s.mu.Lock()
	reply := s.usage
	s.usage = nil
	s.mu.Unlock()
	if reply != nil {
		d := runDecoder{data: frame.Payload}
		lines := d.list()
		if d.err == nil && len(d.data) != 0 {
			d.err = fmt.Errorf("trailing guest disk usage bytes")
		}
		reply <- usageReply{lines: lines, err: d.err}
	}
}

func UsageFrame(lines []string) (Frame, error) {
	var e encoder
	e.list(lines)
	return Frame{Kind: UsageMessage, Payload: e.payload}, e.err
}
