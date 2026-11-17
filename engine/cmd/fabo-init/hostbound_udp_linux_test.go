package main

import (
	"net"
	"net/netip"
	"testing"

	"github.com/OpenFabrica/Fabo/engine/internal/guestwire"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// flow is a conntrack entry of a datagram from src redirected to the relay.
func flow(proto uint8, src string, sport uint16, dst string, dport, rport uint16) *netlink.ConntrackFlow {
	return &netlink.ConntrackFlow{
		Forward: netlink.IPTuple{Protocol: proto, SrcIP: net.ParseIP(src), SrcPort: sport, DstIP: net.ParseIP(dst), DstPort: dport},
		Reverse: netlink.IPTuple{Protocol: proto, SrcIP: net.ParseIP("172.17.0.1"), SrcPort: rport, DstIP: net.ParseIP(src), DstPort: sport},
	}
}

func TestOriginalUDPPort(t *testing.T) {
	host := guestwire.HostRelayAddress
	source := netip.MustParseAddrPort("172.17.0.2:40000")
	flows := []*netlink.ConntrackFlow{
		flow(unix.IPPROTO_TCP, "172.17.0.2", 40000, host, 9001, guestwire.HostRelayPort),
		flow(unix.IPPROTO_UDP, "172.17.0.2", 40000, "8.8.8.8", 53, 53),
		flow(unix.IPPROTO_UDP, "172.17.0.3", 40000, host, 6000, guestwire.HostRelayPort),
		flow(unix.IPPROTO_UDP, "172.17.0.2", 40000, host, 5514, guestwire.HostRelayPort),
	}
	if port, ok := originalUDPPort(flows, source); !ok || port != 5514 {
		t.Fatalf("port %d, %v", port, ok)
	}
	if _, ok := originalUDPPort(flows[:3], source); ok {
		t.Fatal("a flow not redirected to the relay was used")
	}
}
