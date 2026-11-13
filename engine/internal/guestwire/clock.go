package guestwire

import (
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"time"
)

const SetTimeMessage Kind = 18

// SetTimeFrame encodes a host wall-clock sample in protocol v4 format.
func SetTimeFrame(now time.Time) Frame {
	return Frame{Kind: SetTimeMessage, Payload: binary.LittleEndian.AppendUint64(nil, uint64(now.UnixNano()))}
}

// DecodeSetTime validates and decodes the guest wall-clock sample.
func DecodeSetTime(frame Frame) (time.Time, error) {
	if len(frame.Payload) != 8 {
		return time.Time{}, fmt.Errorf("invalid SetTime payload")
	}
	ns := binary.LittleEndian.Uint64(frame.Payload)
	if ns == 0 || ns > math.MaxInt64 {
		return time.Time{}, fmt.Errorf("invalid guest wall time")
	}
	return time.Unix(0, int64(ns)), nil
}

// SetTime queues a wall-clock update without occupying a request slot.
func (s *Session) SetTime(ctx context.Context, now time.Time) error {
	frame := SetTimeFrame(now)
	return s.send(ctx, frame.Kind, encoder{payload: frame.Payload})
}
