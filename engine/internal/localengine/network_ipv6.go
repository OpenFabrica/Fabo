//go:build linux || darwin

package localengine

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"net/netip"
	"slices"

	"github.com/libracoder/kiri/engine/internal/guestwire"
)

type ipv6Pool struct {
	Subnet, IPRange netip.Prefix
	Gateway         netip.Addr
	Auxiliary       map[string]netip.Addr
}

func (p *ipv6Pool) valid() bool {
	if p.Subnet.Bits() < 1 || p.Subnet.Bits() > 126 || p.Subnet != p.Subnet.Masked() || !p.validAddress(p.Gateway) {
		return false
	}
	if p.IPRange.IsValid() && (p.IPRange != p.IPRange.Masked() || p.IPRange.Bits() < p.Subnet.Bits() || !p.Subnet.Contains(p.IPRange.Addr())) {
		return false
	}
	for _, addr := range p.Auxiliary {
		if !p.validAddress(addr) {
			return false
		}
	}
	return true
}

func (p *ipv6Pool) validAddress(addr netip.Addr) bool {
	return addr.Is6() && !addr.Is4In6() && addr.IsGlobalUnicast() && p.Subnet.Contains(addr) && addr != p.Subnet.Addr()
}

func (p *ipv6Pool) available(addr netip.Addr) bool {
	if !p.validAddress(addr) || addr == p.Gateway {
		return false
	}
	for _, reserved := range p.Auxiliary {
		if addr == reserved {
			return false
		}
	}
	return true
}

func (p *ipv6Pool) api() map[string]any {
	config := map[string]any{"Subnet": p.Subnet.String(), "Gateway": p.Gateway.String()}
	if p.IPRange.IsValid() {
		config["IPRange"] = p.IPRange.String()
	}
	if len(p.Auxiliary) != 0 {
		config["AuxiliaryAddresses"] = p.Auxiliary
	}
	return config
}

func parseIPv6Pool(config ipamPoolConfig, subnet netip.Prefix) (*ipv6Pool, error) {
	p := &ipv6Pool{Subnet: subnet, Gateway: subnet.Addr().Next()}
	var err error
	if config.Gateway != "" {
		p.Gateway, err = netip.ParseAddr(config.Gateway)
		if err != nil {
			return nil, fmt.Errorf("%w: invalid IPv6 gateway", errInvalid)
		}
	}
	if config.IPRange != "" {
		p.IPRange, err = netip.ParsePrefix(config.IPRange)
		if err != nil {
			return nil, fmt.Errorf("%w: invalid IPv6 IPRange", errInvalid)
		}
	}
	if len(config.AuxAddresses) != 0 {
		p.Auxiliary = make(map[string]netip.Addr, len(config.AuxAddresses))
		for name, address := range config.AuxAddresses {
			p.Auxiliary[name], err = netip.ParseAddr(address)
			if err != nil {
				return nil, fmt.Errorf("%w: invalid auxiliary IPv6 address %q", errInvalid, address)
			}
		}
	}
	if !p.valid() {
		return nil, fmt.Errorf("%w: invalid IPv6 subnet, gateway or allocation range", errInvalid)
	}
	return p, nil
}

func freeSubnet6(subnet netip.Prefix, networks map[string]dockerNetwork) bool {
	for _, n := range networks {
		if n.IPv6 != nil && n.IPv6.Subnet.Overlaps(subnet) {
			return false
		}
	}
	return true
}

func (e *Engine) automaticSubnet6(name string) netip.Prefix {
	var dir string
	if e.state != nil {
		dir = e.state.dir
	}
	return e.automaticSubnet6From(dir, name)
}

func (e *Engine) automaticSubnet6From(dir, name string) netip.Prefix {
	host := sha256.Sum256([]byte(dir))
	hash := sha256.Sum256([]byte(name))
	start := binary.BigEndian.Uint16(hash[:2])
	address := [16]byte{0xfd, host[0], host[1], host[2], host[3], host[4]}
	for i := range 1 << 16 {
		binary.BigEndian.PutUint16(address[6:8], start+uint16(i))
		subnet := netip.PrefixFrom(netip.AddrFrom16(address), 64)
		if e.freeIPv6Subnet(subnet) {
			return subnet
		}
	}
	return netip.Prefix{}
}

func (e *Engine) allocateIPv6(n dockerNetwork, requested netip.Addr) (netip.Addr, error) {
	pool := n.IPv6
	if pool == nil {
		if requested.IsValid() {
			return netip.Addr{}, fmt.Errorf("%w: network %s does not have IPv6 enabled", errInvalid, n.Name)
		}
		return netip.Addr{}, nil
	}
	used := make(map[netip.Addr]bool)
	for _, c := range e.containers {
		for _, ep := range c.endpoints() {
			if ep.Network.Bridge == n.Bridge {
				ip, _, _ := ep.Network.IPv6()
				used[ip.Addr()] = true
			}
		}
	}
	if requested.IsValid() {
		if !pool.available(requested) {
			return netip.Addr{}, fmt.Errorf("%w: no configured subnet or ip-range contain the IP address %s", errInvalid, requested)
		}
		if used[requested] {
			return netip.Addr{}, fmt.Errorf("%w: Address already in use: %s", errConflict, requested)
		}
		return requested, nil
	}
	allocation := pool.Subnet
	if pool.IPRange.IsValid() {
		allocation = pool.IPRange
	}
	for ip := allocation.Addr(); allocation.Contains(ip); ip = ip.Next() {
		if pool.available(ip) && !used[ip] {
			return ip, nil
		}
	}
	return netip.Addr{}, fmt.Errorf("%w: network %s has no free IPv6 addresses", errConflict, n.Name)
}

func (n dockerNetwork) matchesEndpoint(ep guestwire.Network) bool {
	if n.NoIPv4 != slices.Contains(ep.Options, guestwire.NetworkNoIPv4) {
		return false
	}
	ipv4 := netip.AddrFrom4(ep.IP)
	if !n.NoIPv4 && (!usableAddress(n.Subnet, ipv4) || n.reservedIPv4(ipv4) ||
		n.Gateway.As4() != ep.Gateway || n.Subnet.Bits() != int(ep.Prefix)) {
		return false
	}
	ip, gateway, err := ep.IPv6()
	if err != nil {
		return false
	}
	if n.IPv6 == nil {
		return !ip.IsValid()
	}
	return ip.Bits() == n.IPv6.Subnet.Bits() && n.IPv6.available(ip.Addr()) && gateway == n.IPv6.Gateway
}

func validSavedNetworkPools(n dockerNetwork, networks map[string]dockerNetwork) bool {
	if n.NoIPv4 {
		if n.IPv6 == nil || n.Subnet.IsValid() || n.Gateway.IsValid() || n.IPRange.IsValid() || len(n.Auxiliary) != 0 {
			return false
		}
	} else if !n.validIPv4Pool() || !freeSubnet(n.Subnet, networks) {
		return false
	}
	return n.IPv6 == nil || n.IPv6.valid() && freeSubnet6(n.IPv6.Subnet, networks)
}
