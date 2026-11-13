package guestwire

import (
	"fmt"
	"net/netip"
	"slices"
	"strings"
)

const NetworkNoIPv4 = "noipv4=1"

const networkAddress6 = "ip6="
const networkGateway6 = "gw6="

// IPv6 reads Kiri's existing ip6 and gw6 options for a bridge endpoint.
func (n Network) IPv6() (netip.Prefix, netip.Addr, error) {
	var address, gateway string
	for _, option := range n.Options {
		switch {
		case strings.HasPrefix(option, networkAddress6):
			if address != "" {
				return netip.Prefix{}, netip.Addr{}, fmt.Errorf("duplicate IPv6 endpoint address")
			}
			address = strings.TrimPrefix(option, networkAddress6)
			if address == "" {
				return netip.Prefix{}, netip.Addr{}, fmt.Errorf("empty IPv6 endpoint address")
			}
		case strings.HasPrefix(option, networkGateway6):
			if gateway != "" {
				return netip.Prefix{}, netip.Addr{}, fmt.Errorf("duplicate IPv6 endpoint gateway")
			}
			gateway = strings.TrimPrefix(option, networkGateway6)
			if gateway == "" {
				return netip.Prefix{}, netip.Addr{}, fmt.Errorf("empty IPv6 endpoint gateway")
			}
		}
	}
	if address == "" && gateway == "" {
		return netip.Prefix{}, netip.Addr{}, nil
	}
	ip, e1 := netip.ParsePrefix(address)
	gw, e2 := netip.ParseAddr(gateway)
	if e1 != nil || e2 != nil || !ip.Addr().Is6() || ip.Addr().Is4In6() || gw.Zone() != "" ||
		ip.Bits() < 1 || ip.Bits() > 126 || !ip.Addr().IsGlobalUnicast() || !gw.IsGlobalUnicast() ||
		!ip.Contains(gw) || ip.Addr() == gw || ip.Masked().Addr() == ip.Addr() || ip.Masked().Addr() == gw {
		return netip.Prefix{}, netip.Addr{}, fmt.Errorf("invalid IPv6 bridge endpoint")
	}
	return ip, gw, nil
}

func (n *Network) SetIPv6(addr netip.Prefix, gateway netip.Addr) {
	n.Options = append(n.Options, networkAddress6+addr.String(), networkGateway6+gateway.String())
}

// WithoutIPv6 returns a copy with its IPv6 address and gateway removed.
func (n Network) WithoutIPv6() Network {
	isIPv6 := func(option string) bool {
		return strings.HasPrefix(option, networkAddress6) || strings.HasPrefix(option, networkGateway6)
	}
	if slices.ContainsFunc(n.Options, isIPv6) {
		n.Options = slices.DeleteFunc(slices.Clone(n.Options), isIPv6)
	}
	return n
}

// IPv6Disabled matches Rust Kiri's per-interface IPv6 sysctl handling.
func IPv6Disabled(settings map[string]string, iface string) bool {
	for key, value := range settings {
		if strings.TrimSpace(value) != "1" {
			continue
		}
		switch strings.ReplaceAll(key, "/", ".") {
		case "net.ipv6.conf.all.disable_ipv6", "net.ipv6.conf.default.disable_ipv6", "net.ipv6.conf." + iface + ".disable_ipv6":
			return true
		}
	}
	return false
}
