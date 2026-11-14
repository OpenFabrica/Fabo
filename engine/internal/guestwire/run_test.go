package guestwire

import (
	"bytes"
	"io"
	"net/netip"
	"slices"
	"strings"
	"testing"
)

func TestRunRejectsMismatchedStarted(t *testing.T) {
	var replies, requests bytes.Buffer
	e := encoder{}
	e.text("different")
	e.u32(42)
	if err := WriteFrame(&replies, Frame{Kind: StartedMessage, Payload: e.payload}); err != nil {
		t.Fatal(err)
	}
	called := false
	_, err := RunAttached(struct {
		io.Reader
		io.Writer
	}{bytes.NewReader(replies.Bytes()), &requests}, RunSpec{ID: "expected"}, func(uint32) { called = true }, nil)
	if err == nil || !strings.Contains(err.Error(), "different") || called {
		t.Fatalf("unexpected response: called = %v, err = %v", called, err)
	}
}

func TestRunRejectsOversizedArgument(t *testing.T) {
	_, err := (RunSpec{Argv: []string{strings.Repeat("x", MaxFrame)}}).Frame()
	if err == nil {
		t.Fatal("accepted an argument larger than a frame")
	}
}

func TestRunDNSOptionsRoundTrip(t *testing.T) {
	spec := RunSpec{
		Network:    Network{Options: []string{NetworkInternal}},
		Domainname: "fixture.test",
		DNS: DNSConfig{
			Servers: []netip.Addr{netip.MustParseAddr("192.0.2.53"), netip.MustParseAddr("2001:db8::53")},
			Search:  []string{"fixture.test", "."},
			Options: []string{"ndots:2", "timeout:1"},
		},
	}
	frame, err := spec.Frame()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(frame.Payload, []byte("nameserver=192.0.2.53")) ||
		!bytes.Contains(frame.Payload, []byte("domainname=fixture.test")) {
		t.Fatal("missing Rust network option")
	}
	run, err := DecodeRun(frame)
	if err != nil {
		t.Fatal(err)
	}
	if run.Domainname != spec.Domainname || !slices.Equal(run.Network.Options, spec.Network.Options) || !slices.Equal(run.DNS.Servers, spec.DNS.Servers) ||
		!slices.Equal(run.DNS.Search, spec.DNS.Search) || !slices.Equal(run.DNS.Options, spec.DNS.Options) {
		t.Fatalf("DNS settings or endpoint options changed: %+v", run)
	}
}

func TestProcessOptionsUseRustRunAndExecFields(t *testing.T) {
	process := ProcessOptions{Groups: []string{"share", "2345"}, Ulimits: []Ulimit{
		{Name: "nofile", Soft: 256, Hard: 512},
		{Name: "core", Soft: 0, Hard: -1},
	}}
	frame, err := (RunSpec{Options: []string{"other"}, Process: process}).Frame()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(frame.Payload, []byte("group\tshare")) || !bytes.Contains(frame.Payload, []byte("rlimit\t7\t256\t512")) {
		t.Fatal("missing Rust group or rlimit option")
	}
	run, err := DecodeRun(frame)
	if err != nil || !slices.Equal(run.Process.Groups, process.Groups) || !slices.Equal(run.Process.Ulimits, process.Ulimits) ||
		!slices.Equal(run.Options, []string{"other"}) {
		t.Fatalf("Run changed process settings: %+v, %v", run, err)
	}
	frame, err = (ExecSpec{Options: []string{ExecOutputCreditOption}, Process: process}).Frame()
	if err != nil {
		t.Fatal(err)
	}
	exec, err := DecodeExec(frame)
	if err != nil || !slices.Equal(exec.Process.Groups, process.Groups) || !slices.Equal(exec.Process.Ulimits, process.Ulimits) ||
		!slices.Equal(exec.Options, []string{ExecOutputCreditOption}) {
		t.Fatalf("Exec changed process settings: %+v, %v", exec, err)
	}
}

// Settled carries only the container ID and survives a round trip.
func TestSettledEventRoundTrip(t *testing.T) {
	frame, err := RunEvent{Kind: SettledMessage, ID: "0123456789ab"}.Frame()
	if err != nil {
		t.Fatal(err)
	}
	event, err := DecodeRunEvent(frame)
	if err != nil || event.Kind != SettledMessage || event.ID != "0123456789ab" {
		t.Fatalf("decoded %+v, %v", event, err)
	}
	frame.Payload = append(frame.Payload, 0)
	if _, err := DecodeRunEvent(frame); err == nil {
		t.Fatal("Settled with a payload accepted")
	}
}
