package guestwire

import (
	"encoding/binary"
	"fmt"
	"net"
	"testing"
	"time"
)

func TestSignalLeavesNoPendingReply(t *testing.T) {
	host, guest := net.Pipe()
	defer guest.Close()
	deadline := time.Now().Add(3 * time.Second)
	_ = host.SetDeadline(deadline)
	_ = guest.SetDeadline(deadline)
	s := NewSession(host, nil)
	defer s.Close()
	peer := make(chan error, 1)
	go func() {
		frame, err := ReadFrame(guest, nil)
		if err != nil {
			peer <- err
			return
		}
		id, body, err := takeBytes(frame.Payload)
		if err != nil || frame.Kind != KillMessage || string(id) != "old" || len(body) != 4 || binary.LittleEndian.Uint32(body) != 15 {
			peer <- fmt.Errorf("unexpected signal frame: %+v", frame)
			return
		}
		if _, err := ReadFrame(guest, nil); err != nil {
			peer <- err
			return
		}
		peer <- writeStarted(guest, "new", 42)
	}()
	if err := s.Signal(t.Context(), "old", 15); err != nil {
		t.Fatal(err)
	}
	pid, err := s.Start(t.Context(), RunSpec{ID: "new"})
	if err != nil || pid != 42 {
		t.Fatalf("start after signal: pid=%d err=%v", pid, err)
	}
	if err := <-peer; err != nil {
		t.Fatal(err)
	}
}
