package guestwire

import (
	"context"
	"fmt"
)

const (
	TopQueryMessage Kind = 39
	TopMessage      Kind = 40
)

// Top requests the original Kiri tab-separated process rows.
func (s *Session) Top(ctx context.Context, id string) ([]string, error) {
	var e encoder
	e.text(id)
	if e.err != nil {
		return nil, e.err
	}
	frame, err := s.request(ctx, Frame{Kind: TopQueryMessage, Payload: e.payload}, TopMessage, "", nil, 0)
	if err != nil {
		return nil, err
	}
	d := runDecoder{data: frame.Payload}
	lines := d.list()
	if d.err != nil || len(d.data) != 0 {
		return nil, fmt.Errorf("invalid guest process list")
	}
	return lines, nil
}

// TopFrame encodes the process rows as a single protocol reply.
func TopFrame(lines []string) (Frame, error) {
	var e encoder
	e.list(lines)
	return Frame{Kind: TopMessage, Payload: e.payload}, e.err
}
