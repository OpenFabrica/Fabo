package guestwire

import (
	"context"
	"fmt"
	"io"
)

const (
	InputMessage         Kind = 19
	CloseStdinMessage    Kind = 20
	InputConsumedMessage Kind = 44
	InputChunk                = 64 << 10
	InputWindow               = 4 << 20
)

type inputReceipt struct {
	remaining uint32
	done      chan struct{}
	err       error
}

// WriteInput sends one chunk and waits for the guest to consume it. A canceled
// write retains its receipt until the guest acknowledges it or the run exits.
func (s *Session) WriteInput(ctx context.Context, id string, data []byte) error {
	if len(data) == 0 || len(data) > InputChunk {
		return fmt.Errorf("invalid input chunk size %d", len(data))
	}
	var e encoder
	e.text(id)
	e.u32(uint32(len(data)))
	e.add(data...)
	receipt := &inputReceipt{remaining: uint32(len(data)), done: make(chan struct{})}
	s.mu.Lock()
	if s.inputs[id] != nil {
		s.mu.Unlock()
		return fmt.Errorf("stdin write already pending")
	}
	s.inputs[id] = receipt
	s.mu.Unlock()

	if err := s.send(ctx, InputMessage, e); err != nil {
		s.mu.Lock()
		if s.inputs[id] == receipt {
			delete(s.inputs, id)
		}
		s.mu.Unlock()
		return err
	}
	select {
	case <-receipt.done:
		return receipt.err
	case <-s.done:
		return s.Err()
	case <-ctx.Done():
		return ctx.Err()
	}
}

// CloseStdin asks the guest to deliver EOF after any buffered input.
func (s *Session) CloseStdin(ctx context.Context, id string) error {
	var e encoder
	e.text(id)
	return s.send(ctx, CloseStdinMessage, e)
}

// DecodeInput returns the target ID and a slice backed by the input frame.
func DecodeInput(frame Frame) (string, []byte, error) {
	d := runDecoder{data: frame.Payload}
	id := d.text()
	data, rest, err := takeBytes(d.data)
	if d.err != nil {
		return "", nil, d.err
	}
	if err != nil {
		return "", nil, err
	}
	if len(rest) != 0 {
		return "", nil, fmt.Errorf("trailing bytes in Input")
	}
	return id, data, nil
}

// DecodeCloseStdin returns the container ID whose input should be closed.
func DecodeCloseStdin(frame Frame) (string, error) {
	d := runDecoder{data: frame.Payload}
	id := d.text()
	if len(d.data) != 0 && d.err == nil {
		d.err = fmt.Errorf("trailing bytes in CloseStdin")
	}
	return id, d.err
}

// InputConsumed acknowledges bytes that no longer occupy the guest's input window.
func InputConsumed(id string, size uint32) (Frame, error) {
	var e encoder
	e.text(id)
	e.u32(size)
	return Frame{Kind: InputConsumedMessage, Payload: e.payload}, e.err
}

func (s *Session) acknowledgeInput(frame Frame) error {
	d := runDecoder{data: frame.Payload}
	id := d.text()
	size := uint32(d.number(4))
	if d.err != nil || len(d.data) != 0 || size == 0 {
		return fmt.Errorf("invalid stdin acknowledgment")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	receipt := s.inputs[id]
	if receipt == nil || size > receipt.remaining {
		return fmt.Errorf("unexpected stdin acknowledgment for %q", id)
	}
	receipt.remaining -= size
	if receipt.remaining == 0 {
		delete(s.inputs, id)
		close(receipt.done)
	}
	return nil
}

func (s *Session) endInput(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if receipt := s.inputs[id]; receipt != nil {
		delete(s.inputs, id)
		receipt.err = io.ErrClosedPipe
		close(receipt.done)
	}
}
