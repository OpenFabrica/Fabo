package guestwire

import (
	"bytes"
	"io"
	"net/netip"
	"testing"
)

// The reply ends at the zero frame even when the underlying connection
// stays open, as it can on Virtualization.framework.
func TestFramedRelayEndsWithoutTransportEOF(t *testing.T) {
	data := bytes.Repeat([]byte{1, 2, 3}, 50000)
	var stream bytes.Buffer
	if err := WriteRelayTCP(&stream, bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	if err := stream.WriteByte(9); err != nil {
		t.Fatal(err)
	}
	var received bytes.Buffer
	if err := ReadRelayTCP(&received, &stream); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(received.Bytes(), data) || stream.Len() != 1 {
		t.Fatalf("received %d bytes, stream has %d unread bytes", received.Len(), stream.Len())
	}
}

// Datagram framing preserves message boundaries including an empty packet.
func TestRelayDatagramBoundaries(t *testing.T) {
	var stream bytes.Buffer
	packets := [][]byte{nil, {1, 0, 2}}
	for _, packet := range packets {
		if err := WriteRelayDatagram(&stream, packet); err != nil {
			t.Fatal(err)
		}
	}
	var buf [8]byte
	for _, packet := range packets {
		n, err := ReadRelayDatagram(&stream, buf[:])
		if err != nil || !bytes.Equal(buf[:n], packet) {
			t.Fatalf("datagram = %v, %v", buf[:n], err)
		}
	}
	if _, err := ReadRelayDatagram(&stream, buf[:]); err != io.EOF {
		t.Fatalf("end of stream = %v", err)
	}
}

// IPv6 is a host-to-guest extension. The reverse relay must still reject
// this marker and accept the original IPv4 header.
func TestGuestRelayTargetFamilies(t *testing.T) {
	for _, address := range []string{"127.0.0.1:1234", "[fd00:42::3]:1234"} {
		want := netip.MustParseAddrPort(address)
		var wire bytes.Buffer
		if err := WriteGuestRelayTarget(&wire, want); err != nil {
			t.Fatal(err)
		}
		if got, err := ReadGuestRelayTarget(bytes.NewReader(wire.Bytes())); err != nil || got != want {
			t.Fatalf("guest destination = %s, %v", got, err)
		}
		got, err := ReadRelayTarget(&wire)
		if want.Addr().Is4() {
			if err != nil || got != want {
				t.Fatalf("legacy IPv4 destination = %s, %v", got, err)
			}
		} else if err == nil {
			t.Fatal("host-bound relay accepted the guest IPv6 extension")
		}
	}
}
