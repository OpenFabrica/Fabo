package guestwire

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

func TestSessionInterleavesLifecycleAndStartReplies(t *testing.T) {
	host, guest := net.Pipe()
	deadline := time.Now().Add(3 * time.Second)
	_ = host.SetDeadline(deadline)
	_ = guest.SetDeadline(deadline)
	defer guest.Close()
	events := make(chan RunEvent, 8)
	s := NewSession(host, func(event RunEvent) {
		event.Data = append([]byte(nil), event.Data...)
		events <- event
	})
	defer s.Close()

	peer := make(chan error, 1)
	go func() {
		if _, err := ReadFrame(guest, nil); err != nil {
			peer <- err
			return
		}
		if err := writeStarted(guest, "one", 11); err != nil {
			peer <- err
			return
		}
		if _, err := ReadFrame(guest, nil); err != nil {
			peer <- err
			return
		}
		var out encoder
		out.text("one")
		out.add(1)
		out.u32(5)
		out.add([]byte("hello")...)
		if err := WriteFrame(guest, Frame{Kind: OutputMessage, Payload: out.payload}); err != nil {
			peer <- err
			return
		}
		if err := WriteFrame(guest, Frame{Kind: ErrorMessage, Payload: []byte("start refused")}); err != nil {
			peer <- err
			return
		}
		if _, err := ReadFrame(guest, nil); err != nil {
			peer <- err
			return
		}
		var exit encoder
		exit.text("one")
		exit.u32(42)
		exit.u64(1)
		exit.u64(2)
		if err := WriteFrame(guest, Frame{Kind: ExitedMessage, Payload: exit.payload}); err != nil {
			peer <- err
			return
		}
		peer <- writeStarted(guest, "three", 33)
	}()
	if pid, err := s.Start(t.Context(), RunSpec{ID: "one"}); err != nil || pid != 11 {
		t.Fatalf("first start: pid=%d err=%v", pid, err)
	}
	if _, err := s.Start(t.Context(), RunSpec{ID: "two"}); err == nil || !strings.Contains(err.Error(), "start refused") {
		t.Fatalf("second start: %v", err)
	}
	if pid, err := s.Start(t.Context(), RunSpec{ID: "three"}); err != nil || pid != 33 {
		t.Fatalf("third start: pid=%d err=%v", pid, err)
	}
	if err := <-peer; err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithDeadline(t.Context(), deadline)
	defer cancel()
	for i, want := range []struct {
		kind Kind
		id   string
	}{{StartedMessage, "one"}, {OutputMessage, "one"}, {ExitedMessage, "one"}, {StartedMessage, "three"}} {
		select {
		case got := <-events:
			if got.Kind != want.kind || got.ID != want.id || (i == 1 && string(got.Data) != "hello") || (i == 2 && got.Exit.Code != 42) {
				t.Fatalf("event %d: %+v", i, got)
			}
		case <-ctx.Done():
			t.Fatal("event not delivered")
		}
	}
}

func TestSessionDisconnectReleasesPendingStart(t *testing.T) {
	host, guest := net.Pipe()
	defer guest.Close()
	s := NewSession(host, nil)
	defer s.Close()
	_ = host.SetDeadline(time.Now().Add(3 * time.Second))
	_ = guest.SetDeadline(time.Now().Add(3 * time.Second))
	go func() {
		_, _ = ReadFrame(guest, nil)
		_ = guest.Close()
	}()
	if _, err := s.Start(t.Context(), RunSpec{ID: "pending"}); !errors.Is(err, io.EOF) {
		t.Fatalf("expected EOF, got %v", err)
	}
}

func writeStarted(w io.Writer, id string, pid uint32) error {
	var e encoder
	e.text(id)
	e.u32(pid)
	return WriteFrame(w, Frame{Kind: StartedMessage, Payload: e.payload})
}
