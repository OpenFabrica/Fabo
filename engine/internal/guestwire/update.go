package guestwire

import (
	"context"
	"fmt"
)

const UpdateMessage Kind = 35

func (e *encoder) limits(l Limits) {
	e.u64(l.MemoryMax)
	e.u64(l.CPUQuota)
	e.u32(l.CPUPeriod)
	e.u32(l.CPUWeight)
	e.u32(l.PIDsMax)
	e.u64(l.ShmSize)
}

func (d *runDecoder) limits() Limits {
	return Limits{
		MemoryMax: d.number(8), CPUQuota: d.number(8), CPUPeriod: uint32(d.number(4)),
		CPUWeight: uint32(d.number(4)), PIDsMax: uint32(d.number(4)), ShmSize: d.number(8),
	}
}

// UpdateLimits waits for the guest to apply the container's new limits.
func (s *Session) UpdateLimits(ctx context.Context, id string, limits Limits) error {
	var e encoder
	e.text(id)
	e.limits(limits)
	if e.err != nil {
		return e.err
	}
	_, err := s.request(ctx, Frame{Kind: UpdateMessage, Payload: e.payload}, AckMessage, "", nil, 0)
	return err
}

// DecodeUpdate reads the container ID and full limit set.
func DecodeUpdate(frame Frame) (string, Limits, error) {
	d := runDecoder{data: frame.Payload}
	id, limits := d.text(), d.limits()
	if d.err != nil || len(d.data) != 0 || id == "" {
		return "", Limits{}, fmt.Errorf("invalid Update payload")
	}
	return id, limits, nil
}
