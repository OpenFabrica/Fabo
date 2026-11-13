package guestwire

import "context"

const (
	KillMessage   Kind = 16
	RemoveMessage Kind = 17
)

// Signal queues a signal on the guest connection. The protocol does not
// acknowledge it; container termination is reported independently as Exited.
func (s *Session) Signal(ctx context.Context, id string, signal uint32) error {
	var e encoder
	e.text(id)
	e.u32(signal)
	return s.send(ctx, KillMessage, e)
}

// Remove asks the guest to discard a stopped container's writable layer.
func (s *Session) Remove(ctx context.Context, id string) error {
	var e encoder
	e.text(id)
	return s.send(ctx, RemoveMessage, e)
}

// Shutdown sends the guest its power-off frame. This is used during
// session teardown, when new writes must not extend the shutdown deadline.
func (s *Session) Shutdown(ctx context.Context) error {
	if deadline, ok := ctx.Deadline(); ok {
		if err := s.conn.SetWriteDeadline(deadline); err != nil {
			return err
		}
	}
	return s.send(ctx, Shutdown, encoder{})
}

func (s *Session) send(ctx context.Context, kind Kind, e encoder) error {
	if e.err != nil {
		return e.err
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-s.done:
		return s.Err()
	case s.gate <- struct{}{}:
	}
	defer func() { <-s.gate }()
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := s.Err(); err != nil {
		return err
	}
	if err := WriteFrame(s.conn, Frame{Kind: kind, Payload: e.payload}); err != nil {
		s.fail(err)
		return err
	}
	return nil
}
