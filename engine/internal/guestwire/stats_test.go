package guestwire

import (
	"context"
	"net"
	"testing"
	"time"
)

// Session.Stats asks over the shared session and decodes the reply; a
// pressure notice the guest sends first is an event, not the reply.
func TestSessionStats(t *testing.T) {
	host, guest := net.Pipe()
	defer guest.Close()
	_ = guest.SetDeadline(time.Now().Add(5 * time.Second))
	events := make(chan RunEvent, 4)
	session := NewSession(host, func(event RunEvent) { events <- event })
	defer session.Close()
	want := Stats{UptimeNS: 1, MemoryTotalKiB: 8 << 20, MemoryAvailableKiB: 6 << 20, AgentRSSKiB: 9, MemoryUsedKiB: 700 << 10, MemoryStall: 12, OOMKills: 3}
	go func() {
		frame, err := ReadFrame(guest, nil)
		if err != nil || frame.Kind != StatsRequest {
			return
		}
		_ = WriteFrame(guest, Frame{Kind: MemoryPressure})
		_ = WriteFrame(guest, want.Frame())
	}()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	got, err := session.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("stats %+v, want %+v", got, want)
	}
	select {
	case event := <-events:
		if event.Kind != MemoryPressure {
			t.Fatalf("event kind %d, want MemoryPressure", event.Kind)
		}
	case <-time.After(time.Second):
		t.Fatal("the pressure notice was not delivered as an event")
	}
}

// DecodeStats takes the original four-word sample and rejects other sizes.
func TestDecodeStatsSizes(t *testing.T) {
	short := Stats{UptimeNS: 5, MemoryTotalKiB: 6}.Frame()
	short.Payload = short.Payload[:32]
	if got, err := DecodeStats(short); err != nil || got.MemoryTotalKiB != 6 || got.MemoryUsedKiB != 0 {
		t.Fatalf("four-word sample: %+v, %v", got, err)
	}
	bad := Frame{Kind: StatsMessage, Payload: make([]byte, 12)}
	if _, err := DecodeStats(bad); err == nil {
		t.Fatal("a 12-byte sample decoded")
	}
}
