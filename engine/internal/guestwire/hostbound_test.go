package guestwire

import (
	"bytes"
	"encoding/hex"
	"net/netip"
	"testing"
)

func TestHostBoundRoundTrip(t *testing.T) {
	for _, h := range []HostBound{
		{Source: netip.MustParseAddrPort("172.18.0.3:40512"), Port: 9001},
		{UDP: true, Source: netip.MustParseAddrPort("172.17.0.5:5514"), Port: 5514},
	} {
		var buf bytes.Buffer
		if err := WriteHostBound(&buf, h); err != nil {
			t.Fatal(err)
		}
		got, engine, err := ReadHostBound(&buf)
		if err != nil || engine || got != h {
			t.Fatalf("read %+v %v %v, want %+v", got, engine, err, h)
		}
	}
	// The layout: reserved target 0.0.0.0:2, protocol, source, port.
	var buf bytes.Buffer
	_ = WriteHostBound(&buf, HostBound{UDP: true, Source: netip.MustParseAddrPort("172.18.0.3:258"), Port: 53})
	if got := hex.EncodeToString(buf.Bytes()); got != "000000000002"+"11"+"ac120003"+"0102"+"0035" {
		t.Fatalf("encoding %s", got)
	}
}

func TestHostBoundEngineTarget(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteRelayTarget(&buf, EngineRelayTarget); err != nil {
		t.Fatal(err)
	}
	if _, engine, err := ReadHostBound(&buf); err != nil || !engine {
		t.Fatalf("engine target read as %v, %v", engine, err)
	}
	buf.Reset()
	_ = WriteRelayTarget(&buf, netip.MustParseAddrPort("127.0.0.1:80"))
	if _, _, err := ReadHostBound(&buf); err == nil {
		t.Fatal("a 0.5 header was accepted")
	}
	if err := WriteHostBound(&buf, HostBound{Source: netip.MustParseAddrPort("[fd00::1]:1"), Port: 1}); err == nil {
		t.Fatal("an IPv6 source was written")
	}
}
