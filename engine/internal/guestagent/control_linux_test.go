package guestagent

import (
	"net"
	"testing"
	"time"

	"github.com/OpenFabrica/Fabo/engine/internal/guestwire"
)

func TestControlSamplesAndShutsDown(t *testing.T) {
	host, guest := net.Pipe()
	defer host.Close()
	defer guest.Close()
	deadline := time.Now().Add(3 * time.Second)
	_ = host.SetDeadline(deadline)
	_ = guest.SetDeadline(deadline)
	started, err := BoottimeNS()
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- Serve(guest, started, nil, nil, nil, false) }()
	hello, err := guestwire.ReadHello(host)
	if err != nil || hello.InitStartNS != started || hello.ReadyNS < started || hello.MemoryTotalKiB == 0 {
		t.Fatalf("unexpected hello: %+v, %v", hello, err)
	}
	if err := guestwire.CheckPing(host, 123); err != nil {
		t.Fatal(err)
	}
	sample, err := guestwire.ReadStats(host)
	if err != nil || sample.UptimeNS < hello.ReadyNS || sample.MemoryTotalKiB != hello.MemoryTotalKiB || sample.AgentRSSKiB == 0 {
		t.Fatalf("unexpected stats: %+v, %v", sample, err)
	}
	if err := guestwire.WriteFrame(host, guestwire.Frame{Kind: guestwire.Shutdown}); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
