package guestwire

import (
	"context"
	"fmt"
)

const FreezeMessage Kind = 36

// Freeze waits for the guest to confirm a cgroup freeze or thaw.
func (s *Session) Freeze(ctx context.Context, id string, frozen bool) error {
	var e encoder
	e.text(id)
	if frozen {
		e.add(1)
	} else {
		e.add(0)
	}
	if e.err != nil {
		return e.err
	}
	_, err := s.request(ctx, Frame{Kind: FreezeMessage, Payload: e.payload}, AckMessage, "", nil, 0)
	return err
}

func DecodeFreeze(frame Frame) (id string, frozen bool, err error) {
	d := runDecoder{data: frame.Payload}
	id = d.text()
	value := d.number(1)
	if d.err != nil || len(d.data) != 0 || id == "" || value > 1 {
		return "", false, fmt.Errorf("invalid Freeze payload")
	}
	return id, value == 1, nil
}
