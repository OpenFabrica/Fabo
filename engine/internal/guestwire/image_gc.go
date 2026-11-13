package guestwire

import (
	"context"
	"fmt"
)

// PruneLayersMessage is the acknowledged form of the guest's layer-retention
// operation. The original Kiri RetainLayers command is one-way.
const PruneLayersMessage Kind = 79

func (s *Session) PruneLayers(ctx context.Context, keep []string) error {
	var e encoder
	e.list(keep)
	if e.err != nil {
		return e.err
	}
	_, err := s.request(ctx, Frame{Kind: PruneLayersMessage, Payload: e.payload}, AckMessage, "", nil, 0)
	return err
}

func DecodePruneLayers(frame Frame) ([]string, error) {
	d := runDecoder{data: frame.Payload}
	keep := d.list()
	if d.err == nil && len(d.data) != 0 {
		d.err = fmt.Errorf("trailing layer retention bytes")
	}
	return keep, d.err
}
