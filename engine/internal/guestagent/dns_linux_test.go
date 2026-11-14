package guestagent

import (
	"net/netip"
	"testing"

	"github.com/OpenFabrica/FabriBay/engine/internal/guestwire"
	"golang.org/x/net/dns/dnsmessage"
)

func TestBridgeDNSIsScopedAndForgetsStoppedEndpoints(t *testing.T) {
	a := &bridgeDNS{internal: true, names: make(map[string]map[netip.Addr]struct{})}
	b := &bridgeDNS{internal: true, names: make(map[string]map[netip.Addr]struct{})}
	ipA, ipB := netip.MustParseAddr("10.51.0.20"), netip.MustParseAddr("10.52.0.20")
	a.register([]string{"service"}, ipA)
	ip6 := netip.MustParseAddr("fd00:51::20")
	a.register([]string{"service"}, ip6)
	b.register([]string{"Service"}, ipB)
	ask := func(server *bridgeDNS, kind dnsmessage.Type) dnsmessage.Message {
		t.Helper()
		q := dnsmessage.Message{
			Header: dnsmessage.Header{ID: 5},
			Questions: []dnsmessage.Question{{
				Name: dnsmessage.MustNewName("service."), Type: kind, Class: dnsmessage.ClassINET,
			}},
		}
		wire, err := q.Pack()
		if err != nil {
			t.Fatal(err)
		}
		var answer dnsmessage.Message
		if err := answer.Unpack(server.answer(wire, false)); err != nil {
			t.Fatal(err)
		}
		return answer
	}
	for server, wanted := range map[*bridgeDNS]netip.Addr{a: ipA, b: ipB} {
		answer := ask(server, dnsmessage.TypeA)
		if len(answer.Answers) != 1 || answer.Answers[0].Body.(*dnsmessage.AResource).A != wanted.As4() {
			t.Fatalf("wanted %s, got %+v", wanted, answer)
		}
	}
	answer6 := ask(a, dnsmessage.TypeAAAA)
	if len(answer6.Answers) != 1 || answer6.Answers[0].Body.(*dnsmessage.AAAAResource).AAAA != ip6.As16() {
		t.Fatalf("wanted IPv6 %s, got %+v", ip6, answer6)
	}
	a.unregister([]string{"service"}, ipA)
	a.unregister([]string{"service"}, ip6)
	if answer := ask(a, dnsmessage.TypeAAAA); answer.RCode != dnsmessage.RCodeNameError {
		t.Fatalf("stopped endpoint still resolved: %+v", answer)
	}
	if answer := ask(b, dnsmessage.TypeA); len(answer.Answers) != 1 {
		t.Fatalf("another network lost its endpoint: %+v", answer)
	}
}

func TestResolverContentKeepsLocalDNSAndAllowsHostOverrides(t *testing.T) {
	run := guestwire.RunSpec{
		Network: guestwire.Network{Bridge: guestwire.DefaultBridge},
		DNS: guestwire.DNSConfig{
			Servers: []netip.Addr{netip.MustParseAddr("192.0.2.53")},
			Search:  []string{"."}, Options: []string{"ndots:2"},
		},
	}
	if got := string(resolverContent(nil, run, true)); got != "nameserver 192.0.2.53\noptions ndots:2\n" {
		t.Fatalf("direct resolver: %q", got)
	}
	run.AdditionalNetworks = []guestwire.Network{{Bridge: "fabo1234567890"}}
	if got := string(resolverContent(nil, run, true)); got != "nameserver 127.0.0.11\noptions ndots:2\n" {
		t.Fatalf("embedded resolver: %q", got)
	}
	run.Flags = guestwire.RunNetworkHost
	base := []byte("nameserver 127.0.0.53\nsearch system.test\noptions rotate\n")
	run.DNS.Servers = nil
	if got := string(resolverContent(base, run, true)); got != "nameserver 127.0.0.53\noptions ndots:2\n" {
		t.Fatalf("host resolver: %q", got)
	}
	run.Flags = 0
	run.AdditionalNetworks = nil
	run.Network.Gateway = [4]byte{172, 17, 0, 1}
	if got := string(resolverContent(nil, run, true)); got != "nameserver 172.17.0.1\noptions ndots:2\n" {
		t.Fatalf("VM default bridge resolver: %q", got)
	}
}
