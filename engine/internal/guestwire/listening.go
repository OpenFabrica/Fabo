package guestwire

import (
	"context"
	"fmt"
)

const (
	ListenQueryMessage Kind = 47
	ListeningMessage   Kind = 48
)

// Listening reads the TCP listener rows from a container network namespace.
func (s *Session) Listening(ctx context.Context, id string) ([]string, error) {
	var e encoder
	e.text(id)
	if e.err != nil {
		return nil, e.err
	}
	frame, err := s.request(ctx, Frame{Kind: ListenQueryMessage, Payload: e.payload}, ListeningMessage, "", nil, 0)
	if err != nil {
		return nil, err
	}
	d := runDecoder{data: frame.Payload}
	lines := d.list()
	if d.err != nil || len(d.data) != 0 {
		return nil, fmt.Errorf("invalid guest listener list")
	}
	return lines, nil
}

func ListeningFrame(lines []string) (Frame, error) {
	var e encoder
	e.list(lines)
	return Frame{Kind: ListeningMessage, Payload: e.payload}, e.err
}
