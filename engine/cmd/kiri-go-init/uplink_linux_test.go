package main

import (
	"net"
	"net/netip"
	"slices"
	"testing"
	"time"

	"github.com/insomniacslk/dhcp/dhcpv4"
	"github.com/insomniacslk/dhcp/dhcpv4/nclient4"
)

// A renewal ACK can omit the address, router and DNS. Keep the most
// recently applied values even when they differ from the first offer.
func TestRenewalKeepsUnspecifiedLeaseOptions(t *testing.T) {
	offer, err := dhcpv4.New(
		dhcpv4.WithOption(dhcpv4.OptSubnetMask(net.CIDRMask(16, 32))),
		dhcpv4.WithRouter(net.IPv4(192, 0, 2, 9)), dhcpv4.WithDNS(net.IPv4(192, 0, 2, 9)),
	)
	if err != nil {
		t.Fatal(err)
	}
	ack, err := dhcpv4.New(dhcpv4.WithOption(dhcpv4.OptIPAddressLeaseTime(time.Minute)))
	if err != nil {
		t.Fatal(err)
	}
	before := uplinkConfig{
		address: netip.MustParsePrefix("192.0.2.4/24"), router: netip.MustParseAddr("192.0.2.1"),
		dns: []netip.Addr{netip.MustParseAddr("192.0.2.2")}, lifetime: time.Hour,
	}
	now := time.Now()
	after, err := readUplinkConfig(&nclient4.Lease{Offer: offer, ACK: ack, CreationTime: now}, before)
	if err != nil {
		t.Fatal(err)
	}
	if after.address != before.address || after.router != before.router || !slices.Equal(after.dns, before.dns) {
		t.Fatalf("renewal replaced active options: %+v", after)
	}
	if after.expires.Sub(now) != time.Minute || after.renewAt.Sub(now) != 30*time.Second {
		t.Fatalf("renewal schedule does not follow the new lease: %+v", after)
	}
}
