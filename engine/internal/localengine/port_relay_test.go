//go:build linux || darwin

package localengine

import (
	"errors"
	"net"
	"net/netip"
	"testing"
)

// A conflict on a later binding must leave earlier bindings available.
func TestPortReservationRollsBack(t *testing.T) {
	ip := netip.MustParseAddr("127.0.0.1")
	occupied, err := net.ListenTCP("tcp4", net.TCPAddrFromAddrPort(netip.AddrPortFrom(ip, 0)))
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	conflict := occupied.Addr().(*net.TCPAddr).AddrPort().Port()
	udp, err := net.ListenUDP("udp4", net.UDPAddrFromAddrPort(netip.AddrPortFrom(ip, 0)))
	if err != nil {
		t.Fatal(err)
	}
	free := udp.LocalAddr().(*net.UDPAddr).AddrPort().Port()
	_ = udp.Close()

	bindings := []portBinding{{80, "udp", ip.String(), free}, {80, "tcp", ip.String(), conflict}}
	p, err := reservePorts(bindings, ip, nil)
	if p != nil {
		p.close()
	}
	if !errors.Is(err, errConflict) {
		t.Fatalf("expected conflict, got %v", err)
	}
	rebound, err := net.ListenUDP("udp4", net.UDPAddrFromAddrPort(netip.AddrPortFrom(ip, free)))
	if err != nil {
		t.Fatalf("earlier binding was not released: %v", err)
	}
	_ = rebound.Close()
}
