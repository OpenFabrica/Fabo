//go:build linux || darwin

package localengine

import (
	"fmt"
	"maps"
	"net"
	"net/netip"
	"slices"
	"strings"
	"time"

	"github.com/OpenFabrica/FabriBay/engine/internal/guestwire"
)

var defaultSubnet = netip.MustParsePrefix("172.17.0.0/16")

const containerNetworkPrefix = "container:"

type dockerNetwork struct {
	ID, Name, Bridge     string
	Subnet               netip.Prefix
	Gateway              netip.Addr
	IPRange              netip.Prefix          `json:",omitzero"`
	Auxiliary            map[string]netip.Addr `json:",omitempty"`
	IPv6                 *ipv6Pool             `json:",omitempty"`
	NoIPv4               bool                  `json:",omitempty"`
	Internal, Attachable bool
	Created              time.Time
	Labels               map[string]string
	Options, IPAMOptions map[string]string `json:",omitempty"`
}

func bridgeNetwork() dockerNetwork {
	return dockerNetwork{ID: "bridge", Name: "bridge", Bridge: guestwire.DefaultBridge, Subnet: defaultSubnet, Gateway: defaultSubnet.Addr().Next()}
}

func hostNetwork() dockerNetwork {
	return dockerNetwork{ID: "host", Name: "host"}
}

func noneNetwork() dockerNetwork {
	return dockerNetwork{ID: "none", Name: "none", Internal: true}
}

func (e *Engine) network(key string) (dockerNetwork, error) {
	switch key {
	case "", "bridge", "default":
		return e.bridgeNetwork(), nil
	case "none":
		return noneNetwork(), nil
	case "host":
		return hostNetwork(), nil
	}
	if n, ok := e.networks[key]; ok {
		return n, nil
	}
	var found dockerNetwork
	for _, n := range e.networks {
		if n.ID == key {
			return n, nil
		}
		if strings.HasPrefix(n.ID, key) {
			if found.ID != "" {
				return dockerNetwork{}, fmt.Errorf("%w: ambiguous network %q", errConflict, key)
			}
			found = n
		}
	}
	if found.ID == "" {
		return dockerNetwork{}, fmt.Errorf("%w: network %q", errMissing, key)
	}
	return found, nil
}

func usableAddress(subnet netip.Prefix, ip netip.Addr) bool {
	return ip.Is4() && ip.IsGlobalUnicast() && subnet.Contains(ip) && ip != subnet.Masked().Addr() && subnet.Contains(ip.Next())
}

func (n dockerNetwork) validIPv4Pool() bool {
	if !n.Subnet.Addr().Is4() || n.Subnet != n.Subnet.Masked() || n.Subnet.Bits() < 1 || n.Subnet.Bits() > 30 || !usableAddress(n.Subnet, n.Gateway) {
		return false
	}
	if n.IPRange.IsValid() && (n.IPRange != n.IPRange.Masked() || n.IPRange.Bits() < n.Subnet.Bits() || !n.Subnet.Contains(n.IPRange.Addr())) {
		return false
	}
	for _, addr := range n.Auxiliary {
		if !n.Subnet.Contains(addr) {
			return false
		}
	}
	return true
}

func (n dockerNetwork) reservedIPv4(ip netip.Addr) bool {
	if ip == n.Gateway {
		return true
	}
	for _, addr := range n.Auxiliary {
		if ip == addr {
			return true
		}
	}
	return false
}

// allocateNetwork is called with e.mu held. Stopped containers retain their
// reservation until removed, so the same endpoint is restored on start.
func (e *Engine) allocateNetwork(n dockerNetwork, requested netip.Addr) (guestwire.Network, error) {
	used := map[netip.Addr]bool{n.Gateway: true}
	for _, addr := range n.Auxiliary {
		used[addr] = true
	}
	for _, c := range e.containers {
		for _, endpoint := range c.endpoints() {
			if endpoint.Network.Bridge == n.Bridge {
				used[netip.AddrFrom4(endpoint.Network.IP)] = true
			}
		}
	}
	ip := requested
	if ip.IsValid() {
		if n.Bridge == guestwire.DefaultBridge {
			return guestwire.Network{}, fmt.Errorf("%w: user specified IP address is supported on user defined networks only", errInvalid)
		}
		if !usableAddress(n.Subnet, ip) {
			return guestwire.Network{}, fmt.Errorf("%w: no configured subnet or ip-range contain the IP address %s", errInvalid, ip)
		}
		if used[ip] {
			return guestwire.Network{}, fmt.Errorf("%w: Address already in use: %s", errConflict, ip)
		}
	} else {
		allocation := n.Subnet
		if n.IPRange.IsValid() {
			allocation = n.IPRange
		}
		ip = allocation.Addr()
		if ip == n.Subnet.Addr() {
			ip = ip.Next()
		}
		for allocation.Contains(ip) && usableAddress(n.Subnet, ip) {
			if !used[ip] {
				break
			}
			ip = ip.Next()
		}
		if !allocation.Contains(ip) || !usableAddress(n.Subnet, ip) {
			return guestwire.Network{}, fmt.Errorf("%w: network %s has no free IPv4 addresses", errConflict, n.Name)
		}
	}
	result := guestwire.Network{Bridge: n.Bridge, IP: ip.As4(), Prefix: uint8(n.Subnet.Bits()), Gateway: n.Gateway.As4()}
	if n.Internal {
		result.Options = []string{guestwire.NetworkInternal}
	}
	return result, nil
}

type containerEndpoint struct {
	Name, ID, Interface string
	Network             guestwire.Network
}

func (c *container) endpoints() []containerEndpoint {
	if c.run.Flags&guestwire.RunNetworkHost != 0 {
		return []containerEndpoint{{Name: "host", ID: "host"}}
	}
	if c.run.Network.Bridge == "" {
		return nil
	}
	id, ifname := c.networkID, c.networkInterface
	if id == "" {
		id = "bridge"
	}
	if ifname == "" {
		ifname = "eth0"
	}
	primary := c.run.Network
	primary.ExtraHosts = nil
	return append([]containerEndpoint{{c.networkMode(), id, ifname, primary}}, c.extraNetworks...)
}

func (c *container) setEndpoints(endpoints []containerEndpoint) {
	extraHosts := c.run.Network.ExtraHosts
	c.run.Flags &^= guestwire.RunNetworkHost | guestwire.RunNetworkNone
	if len(endpoints) == 0 {
		c.run.Network, c.extraNetworks = guestwire.Network{ExtraHosts: extraHosts}, nil
		c.networkID, c.networkInterface = "", ""
		if c.networkTarget() == "" {
			c.networkName = "none"
			c.run.Flags |= guestwire.RunNetworkNone
		}
		return
	}
	primary := endpoints[0]
	if primary.Name == "host" {
		c.run.Flags |= guestwire.RunNetworkHost
	}
	c.run.Network, c.extraNetworks = primary.Network, slices.Clone(endpoints[1:])
	c.run.Network.ExtraHosts = extraHosts
	c.networkName, c.networkID, c.networkInterface = primary.Name, primary.ID, primary.Interface
}

func (e *Engine) newEndpoint(n dockerNetwork, config endpointConfig, name, id, ifname string) (containerEndpoint, error) {
	if config.networkID != "" && config.networkID != n.ID {
		return containerEndpoint{}, fmt.Errorf("%w: mismatched network ID", errInvalid)
	}
	network := guestwire.Network{Bridge: n.Bridge}
	if n.NoIPv4 {
		if config.ip.IsValid() {
			return containerEndpoint{}, fmt.Errorf("%w: network %s has no IPv4 subnet", errInvalid, n.Name)
		}
		network.Options = []string{guestwire.NetworkNoIPv4}
		if n.Internal {
			network.Options = append(network.Options, guestwire.NetworkInternal)
		}
	} else {
		var err error
		network, err = e.allocateNetwork(n, config.ip)
		if err != nil {
			return containerEndpoint{}, err
		}
	}
	ip6, err := e.allocateIPv6(n, config.ip6)
	if err != nil {
		return containerEndpoint{}, err
	}
	if ip6.IsValid() {
		network.SetIPv6(netip.PrefixFrom(ip6, n.IPv6.Subnet.Bits()), n.IPv6.Gateway)
	}
	if guestwire.CustomBridge(n.Bridge) {
		network.DNSNames = append([]string{name, id[:12]}, config.aliases...)
	}
	network.StaticIPv4, network.StaticIPv6 = config.ip.IsValid(), config.ip6.IsValid()
	network.SetMAC(config.mac)
	network.SetGatewayPriority(config.priority)
	return containerEndpoint{n.Name, n.ID, ifname, network}, nil
}

// initialEndpoints resolves names and IDs together before reserving any
// addresses. The requested primary remains eth0 for existing port bindings.
func (e *Engine) initialEndpoints(mode string, requested map[string]endpointConfig, legacyMAC net.HardwareAddr, name, id string) ([]containerEndpoint, error) {
	if target, join := strings.CutPrefix(mode, containerNetworkPrefix); join {
		if !validBlobName(target) || target == id || len(requested) != 0 {
			return nil, fmt.Errorf("%w: invalid shared container network", errInvalid)
		}
		return nil, nil
	}
	primary, err := e.network(mode)
	if err != nil {
		return nil, err
	}
	if primary.Bridge == "" {
		for name, config := range requested {
			if name != primary.Name || config.ip.IsValid() || config.ip6.IsValid() || len(config.mac) != 0 || len(config.aliases)+len(config.links) != 0 || config.priority != 0 ||
				(config.networkID != "" && config.networkID != primary.ID) {
				return nil, fmt.Errorf("%w: network %s cannot be combined with bridge endpoints", errInvalid, primary.Name)
			}
		}
		if primary.Name == "host" {
			return []containerEndpoint{{Name: primary.Name, ID: primary.ID}}, nil
		}
		return nil, nil
	}
	networks := []dockerNetwork{primary}
	configs := map[string]endpointConfig{}
	for _, key := range slices.Sorted(maps.Keys(requested)) {
		n, err := e.network(key)
		if err != nil {
			return nil, err
		}
		if n.Bridge == "" {
			return nil, fmt.Errorf("%w: network %s cannot be combined with bridge endpoints", errInvalid, n.Name)
		}
		if _, duplicate := configs[n.ID]; duplicate {
			return nil, fmt.Errorf("%w: duplicate endpoint for %q", errInvalid, n.Name)
		}
		configs[n.ID] = requested[key]
		if n.ID != primary.ID {
			networks = append(networks, n)
		}
	}
	if len(networks) > guestwire.MaxEndpoints {
		return nil, fmt.Errorf("%w: too many networks", errInvalid)
	}
	var endpoints []containerEndpoint
	for i, n := range networks {
		config := configs[n.ID]
		if i == 0 && len(config.mac) == 0 {
			config.mac = legacyMAC
		}
		ep, err := e.newEndpoint(n, config, name, id, fmt.Sprintf("eth%d", i))
		if err != nil {
			return nil, err
		}
		endpoints = append(endpoints, ep)
	}
	return endpoints, nil
}

func (c *container) networkMode() string {
	if c.networkName != "" {
		return c.networkName
	}
	if c.run.Flags&guestwire.RunNetworkHost != 0 {
		return "host"
	}
	if c.run.Flags&guestwire.RunNetworkNone != 0 {
		return "none"
	}
	return "bridge"
}

// pidMode is HostConfig.PidMode as Docker reports it.
func (c *container) pidMode() string {
	if target := c.pidTarget(); target != "" {
		return containerNetworkPrefix + target
	}
	return ""
}

// pidTarget is the ID of the container whose PID namespace this one joins.
func (c *container) pidTarget() string {
	target, _ := guestwire.SplitPIDJoin(c.run.Options)
	return target
}

func (c *container) networkTarget() string {
	if target, join := strings.CutPrefix(c.networkName, containerNetworkPrefix); join {
		return target
	}
	return ""
}

func (e *Engine) networkSettings(c *container) map[string]any {
	var bound []portBinding
	if c.state == "running" && c.attempt != nil && c.attempt.ports != nil {
		bound = c.attempt.ports.bound
	}
	networks := map[string]any{}
	settings := map[string]any{"Networks": networks, "Ports": dockerBindings(bound, c.exposedPorts)}
	for _, ep := range c.endpoints() {
		n := e.liveNetwork(ep.Network)
		address, gateway, prefix := "", "", uint8(0)
		if c.state == "running" && n.Prefix != 0 {
			address = netip.AddrFrom4(n.IP).String()
			if !slices.Contains(n.Options, guestwire.NetworkInternal) {
				gateway = netip.AddrFrom4(n.Gateway).String()
			}
			prefix = n.Prefix
		}
		address6, gateway6, prefix6 := "", "", 0
		if c.state == "running" && !guestwire.IPv6Disabled(c.run.Sysctls, ep.Interface) {
			if ip, gw, _ := n.IPv6(); ip.IsValid() {
				address6, prefix6 = ip.Addr().String(), ip.Bits()
				if !slices.Contains(n.Options, guestwire.NetworkInternal) {
					gateway6 = gw.String()
				}
			}
		}
		priority, _ := n.GatewayPriority()
		mac, _ := n.MAC()
		var ipam map[string]string
		if ep.Network.StaticIPv4 || ep.Network.StaticIPv6 {
			ipam = make(map[string]string)
			if ep.Network.StaticIPv4 {
				ipam["IPv4Address"] = netip.AddrFrom4(ep.Network.IP).String()
			}
			if ep.Network.StaticIPv6 {
				ip, _, _ := ep.Network.IPv6()
				ipam["IPv6Address"] = ip.Addr().String()
			}
		}
		endpoint := map[string]any{
			"NetworkID": ep.ID, "EndpointID": c.id, "IPAddress": address, "Gateway": gateway, "IPPrefixLen": prefix,
			"Aliases": n.DNSNames, "GwPriority": priority, "Links": c.inspectLinks(ep.ID),
			"MacAddress": mac.String(), "IPAMConfig": ipam,
			"GlobalIPv6Address": address6, "GlobalIPv6PrefixLen": prefix6, "IPv6Gateway": gateway6,
		}
		networks[ep.Name] = endpoint
		if ep.Name == "bridge" && c.networkMode() == "bridge" {
			for _, key := range []string{"EndpointID", "IPAddress", "Gateway", "IPPrefixLen", "MacAddress", "GlobalIPv6Address", "GlobalIPv6PrefixLen", "IPv6Gateway"} {
				settings[key] = endpoint[key]
			}
		}
	}
	return settings
}

// networkAddress is the target of Fabo's port and HTTP domain relays.
func (c *container) networkAddress() netip.Addr {
	if c.run.Flags&guestwire.RunNetworkHost != 0 {
		return netip.AddrFrom4([4]byte{127, 0, 0, 1})
	}
	if c.run.Network.IP != [4]byte{} && !slices.Contains(c.run.Network.Options, guestwire.NetworkNoIPv4) {
		return netip.AddrFrom4(c.run.Network.IP)
	}
	if !guestwire.IPv6Disabled(c.run.Sysctls, c.networkInterface) {
		if address, _, _ := c.run.Network.IPv6(); address.IsValid() {
			return address.Addr()
		}
	}
	return netip.Addr{}
}
