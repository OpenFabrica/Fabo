package guestwire

import (
	"context"
	"net"
	"slices"
	"testing"
	"time"
)

func TestUsageDoesNotBlockControlRequests(t *testing.T) {
	host, guest := net.Pipe()
	t.Cleanup(func() { _ = host.Close(); _ = guest.Close() })
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	if err := guest.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	session := NewSession(host, nil)
	done := make(chan usageReply, 1)
	go func() {
		lines, err := session.Usage(ctx)
		done <- usageReply{lines: lines, err: err}
	}()
	frame, err := ReadFrame(guest, nil)
	if err != nil || frame.Kind != UsageQueryMessage || len(frame.Payload) != 0 {
		t.Fatalf("usage query: %+v, %v", frame, err)
	}
	control := make(chan error, 1)
	go func() { control <- session.DeleteVolume(ctx, "volume") }()
	frame, err = ReadFrame(guest, nil)
	if err != nil || frame.Kind != DeleteVolumeMessage {
		t.Fatalf("control blocked by usage: %+v, %v", frame, err)
	}
	if err := WriteFrame(guest, Frame{Kind: AckMessage}); err != nil {
		t.Fatal(err)
	}
	if err := <-control; err != nil {
		t.Fatal(err)
	}
	lines := []string{"l digest 4096", "v volume 8192"}
	frame, err = UsageFrame(lines)
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteFrame(guest, frame); err != nil {
		t.Fatal(err)
	}
	result := <-done
	if result.err != nil || !slices.Equal(result.lines, lines) {
		t.Fatalf("usage reply: %+v", result)
	}
}
