package guestwire

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"sync"
)

type replyResult struct {
	frame Frame
	err   error
}

type requestSlot struct {
	id     string
	kind   Kind
	result chan replyResult
}

// Session owns a control connection after Hello. One reader dispatches
// lifecycle events in wire order. The callback must return promptly and must
// not call Session methods that wait for replies. Copy event data to retain it.
type Session struct {
	conn   net.Conn
	events func(RunEvent)
	gate   chan struct{}
	done   chan struct{}

	mu      sync.Mutex
	pending *requestSlot
	usage   chan usageReply
	inputs  map[string]*inputReceipt
	err     error
}

func NewSession(conn net.Conn, events func(RunEvent)) *Session {
	s := &Session{conn: conn, events: events, gate: make(chan struct{}, 1), done: make(chan struct{}), inputs: make(map[string]*inputReceipt)}
	go s.read()
	return s
}

func (s *Session) Done() <-chan struct{} { return s.done }

func (s *Session) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

func (s *Session) Close() error { return s.conn.Close() }

// Start waits for the guest's startup reply. The context controls admission
// only: once submitted, the request must consume its reply because guest
// errors have no ID. Closing the session interrupts an outstanding call.
func (s *Session) Start(ctx context.Context, spec RunSpec) (uint32, error) {
	frame, err := spec.Frame()
	if err != nil {
		return 0, err
	}
	reply, err := s.request(ctx, frame, StartedMessage, spec.ID, nil, 0)
	if err != nil {
		return 0, err
	}
	event, err := DecodeRunEvent(reply)
	return event.PID, err
}

// request keeps the command frame and any following raw bytes together.
// Once writing begins, the response must be consumed before admitting
// another command. A short blob invalidates the connection.
func (s *Session) request(ctx context.Context, frame Frame, kind Kind, id string, blob io.Reader, size int64) (Frame, error) {
	select {
	case <-ctx.Done():
		return Frame{}, ctx.Err()
	case <-s.done:
		return Frame{}, s.Err()
	case s.gate <- struct{}{}:
	}
	defer func() { <-s.gate }()
	if err := ctx.Err(); err != nil {
		return Frame{}, err
	}

	request := &requestSlot{id: id, kind: kind, result: make(chan replyResult, 1)}
	s.mu.Lock()
	if s.err != nil {
		err := s.err
		s.mu.Unlock()
		return Frame{}, err
	}
	s.pending = request
	s.mu.Unlock()
	if err := WriteFrame(s.conn, frame); err != nil {
		s.fail(err)
		return Frame{}, err
	}
	if blob != nil {
		if _, err := io.CopyN(s.conn, blob, size); err != nil {
			err = fmt.Errorf("send guest blob: %w", err)
			s.fail(err)
			return Frame{}, s.Err()
		}
	}
	select {
	case result := <-request.result:
		return result.frame, result.err
	case <-s.done:
		// If the guest acknowledged the request and then disconnected,
		// return the actual reply instead of the later connection error.
		select {
		case result := <-request.result:
			return result.frame, result.err
		default:
			return Frame{}, s.Err()
		}
	}
}

func (s *Session) read() {
	var scratch []byte
	for {
		frame, err := ReadFrame(s.conn, scratch)
		if err != nil {
			s.fail(err)
			return
		}
		scratch = frame.Payload
		switch frame.Kind {
		case MemoryPressure:
			s.emit(RunEvent{Kind: frame.Kind})
		case ErrorMessage, LayerStatusMessage, LayerStoredMessage, AckMessage, ContainerStatsMessage, TopMessage, ListeningMessage,
			FileStatReply, FileListReply, FileReadReply:
			if err := s.reply("", frame); err != nil {
				s.fail(err)
				return
			}
		case UsageMessage:
			s.acceptUsage(frame)
		case InputConsumedMessage:
			if err := s.acknowledgeInput(frame); err != nil {
				s.fail(err)
				return
			}
		case StartedMessage, SettledMessage, OutputMessage, ExitedMessage:
			event, err := DecodeRunEvent(frame)
			if err != nil {
				s.fail(err)
				return
			}
			if event.Kind == StartedMessage {
				if err := s.reply(event.ID, frame); err != nil {
					s.fail(err)
					return
				}
			}
			if event.Kind == ExitedMessage {
				s.endInput(event.ID)
			}
			s.emit(event)
		default:
			s.fail(fmt.Errorf("unsupported guest message %d", frame.Kind))
			return
		}
	}
}

func (s *Session) emit(event RunEvent) {
	if s.events != nil {
		s.events(event)
	}
}

func (s *Session) reply(id string, frame Frame) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pending == nil {
		return fmt.Errorf("guest replied without a pending request")
	}
	result := replyResult{}
	if frame.Kind == ErrorMessage {
		result.err = expect(frame, s.pending.kind)
		// A layer error can happen before the guest drains the raw blob.
		// Its next frame boundary is unknown, so end the session.
		if s.pending.kind == LayerStoredMessage {
			return result.err
		}
	} else {
		if frame.Kind != s.pending.kind {
			return fmt.Errorf("guest replied with %d, expected %d", frame.Kind, s.pending.kind)
		}
		if id != s.pending.id {
			return fmt.Errorf("guest replied for %q, expected %q", id, s.pending.id)
		}
		result.frame = Frame{Kind: frame.Kind, Payload: bytes.Clone(frame.Payload)}
	}
	s.pending.result <- result
	s.pending = nil
	return nil
}

func (s *Session) fail(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err == nil {
		s.err = err
		s.pending = nil
		close(s.done)
		_ = s.conn.Close()
	}
}
