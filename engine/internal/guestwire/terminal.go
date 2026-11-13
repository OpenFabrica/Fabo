package guestwire

import (
	"context"
	"encoding/binary"
	"fmt"
)

const ResizeMessage Kind = 21

// Resize updates the guest terminal identified by id. It has no reply.
func (s *Session) Resize(ctx context.Context, id string, rows, cols uint16) error {
	var e encoder
	e.text(id)
	var size [4]byte
	binary.LittleEndian.PutUint16(size[:2], rows)
	binary.LittleEndian.PutUint16(size[2:], cols)
	e.add(size[:]...)
	return s.send(ctx, ResizeMessage, e)
}

// DecodeResize reads the target and the terminal dimensions in rows and columns.
func DecodeResize(frame Frame) (string, uint16, uint16, error) {
	d := runDecoder{data: frame.Payload}
	id := d.text()
	if d.err != nil {
		return "", 0, 0, d.err
	}
	if id == "" || len(d.data) != 4 {
		return "", 0, 0, fmt.Errorf("invalid Resize payload")
	}
	return id, binary.LittleEndian.Uint16(d.data[:2]), binary.LittleEndian.Uint16(d.data[2:]), nil
}
