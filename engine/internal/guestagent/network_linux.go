package guestagent

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"slices"
	"strings"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"

	"github.com/OpenFabrica/Fabo/engine/internal/guestwire"
)

type bridgeNetwork struct {
	link           netlink.Link
	gateway        netip.Prefix
	gateway6       netip.Prefix
	stopForwarding func()
	internal       bool
	dns            *bridgeDNS
}

func validateNetwork(n guestwire.Network) error {
	custom := guestwire.CustomBridge(n.Bridge)
	if len(n.ExtraHosts) != 0 || (!custom && (len(n.DNSNames) != 0 || slices.Contains(n.Options, guestwire.NetworkInternal))) {
		return fmt.Errorf("unsupported guest network options")
	}
	if _, err := n.GatewayPriority(); err != nil {
		return err
	}
	if !custom && n.Bridge != guestwire.DefaultBridge {
		return fmt.Errorf("invalid bridge name")
	}
	ip6, _, err := n.IPv6()
	if err != nil {
		return err
	}
	if slices.Contains(n.Options, guestwire.NetworkNoIPv4) {
		if !ip6.IsValid() {
			return fmt.Errorf("IPv6-only bridge has no IPv6 endpoint")
		}
		return nil
	}
	ip, gateway := netip.AddrFrom4(n.IP), netip.AddrFrom4(n.Gateway)
	subnet := netip.PrefixFrom(gateway, int(n.Prefix))
	if n.Prefix < 1 || n.Prefix > 30 ||
		!ip.IsGlobalUnicast() || !gateway.IsGlobalUnicast() || !subnet.Contains(ip) || ip == gateway ||
		ip == subnet.Masked().Addr() || gateway == subnet.Masked().Addr() {
		return fmt.Errorf("invalid bridge endpoint")
	}
	mask := net.CIDRMask(int(n.Prefix), 32)
	hostBits := func(addr [4]byte) bool {
		for i, b := range mask {
			if addr[i]|b != 255 {
				return false
			}
		}
		return true
	}
	if hostBits(n.IP) || hostBits(n.Gateway) {
		return fmt.Errorf("bridge endpoint uses a broadcast address")
	}
	return nil
}

func bridgeGateways(n guestwire.Network) (netip.Prefix, netip.Prefix) {
	var v4, v6 netip.Prefix
	if !slices.Contains(n.Options, guestwire.NetworkNoIPv4) {
		v4 = netip.PrefixFrom(netip.AddrFrom4(n.Gateway), int(n.Prefix))
	}
	if ip, gateway, _ := n.IPv6(); ip.IsValid() {
		v6 = netip.PrefixFrom(gateway, ip.Bits())
	}
	return v4, v6
}

func bridgeRoutes(gateway netip.Prefix) ([]netlink.Route, error) {
	if !gateway.IsValid() {
		return nil, nil
	}
	family := netlink.FAMILY_V4
	if gateway.Addr().Is6() {
		family = netlink.FAMILY_V6
	}
	routes, err := netlink.RouteList(nil, family)
	if err != nil {
		return nil, err
	}
	subnet := gateway.Masked()
	for _, route := range routes {
		if route.Dst == nil {
			continue
		}
		prefix, err := netip.ParsePrefix(route.Dst.String())
		if err == nil && prefix.Bits() > 0 && prefix.Overlaps(subnet) {
			return nil, fmt.Errorf("bridge subnet %s overlaps existing route %s", subnet, prefix)
		}
	}
	return routes, nil
}

func addressMAC(addr netip.Addr, tag byte) net.HardwareAddr {
	if addr.Is4() {
		b := addr.As4()
		return net.HardwareAddr{0x02, tag, b[0], b[1], b[2], b[3]}
	}
	hash := sha256.Sum256(addr.AsSlice())
	return net.HardwareAddr{0x02, tag, hash[0], hash[1], hash[2], hash[3]}
}

func (r *Runtime) openBridge(n guestwire.Network) (*bridgeNetwork, error) {
	gateway, gateway6 := bridgeGateways(n)
	routes, err := bridgeRoutes(gateway)
	if err != nil {
		return nil, err
	}
	routes6, err := bridgeRoutes(gateway6)
	if err != nil {
		return nil, err
	}
	listen := gateway
	if !listen.IsValid() {
		listen = gateway6
	}
	attrs := netlink.NewLinkAttrs()
	attrs.Name = n.Bridge
	attrs.HardwareAddr = addressMAC(listen.Addr(), 0x4b)
	bridge := &netlink.Bridge{LinkAttrs: attrs}
	if err := netlink.LinkAdd(bridge); err != nil {
		return nil, fmt.Errorf("create bridge %s: %w", n.Bridge, err)
	}
	internal := slices.Contains(n.Options, guestwire.NetworkInternal)
	var stops []func()
	stop := func() {
		for _, undo := range slices.Backward(stops) {
			undo()
		}
	}
	configure := func() error {
		path := "/proc/sys/net/ipv6/conf/" + n.Bridge + "/disable_ipv6"
		if gateway6.IsValid() {
			if err := os.WriteFile(path, []byte("0"), 0); err != nil {
				return err
			}
		} else if err := os.WriteFile(path, []byte("1"), 0); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if !gateway.IsValid() {
			// An IPv6-only bridge must still block IPv4 forwarding when
			// the Linux host already forwards IPv4 on other interfaces.
			undo, err := r.forwarding.forwardBridge(bridge, netip.Prefix{}, nil, true, false)
			if err != nil {
				return err
			}
			stops = append(stops, undo)
		}
		for _, entry := range []struct {
			gateway netip.Prefix
			routes  []netlink.Route
		}{{gateway, routes}, {gateway6, routes6}} {
			if !entry.gateway.IsValid() {
				continue
			}
			addr := entry.gateway.Addr()
			info := &netlink.Addr{IPNet: ipNetwork(addr, entry.gateway.Bits())}
			if addr.Is6() {
				info.Flags = unix.IFA_F_NODAD
			}
			if err := netlink.AddrAdd(bridge, info); err != nil {
				return err
			}
			undo, err := r.forwarding.forwardBridge(bridge, entry.gateway.Masked(), entry.routes, internal, r.vm)
			if err != nil {
				return err
			}
			stops = append(stops, undo)
		}
		return netlink.LinkSetUp(bridge)
	}
	if err := configure(); err != nil {
		stop()
		return nil, errors.Join(err, netlink.LinkDel(bridge))
	}
	result := &bridgeNetwork{link: bridge, gateway: gateway, gateway6: gateway6, stopForwarding: stop, internal: internal}
	if guestwire.CustomBridge(n.Bridge) || r.vm {
		result.dns, err = listenBridgeDNS(listen, internal, r.vm)
		if err != nil {
			stop()
			return nil, errors.Join(fmt.Errorf("listen for bridge DNS: %w", err), netlink.LinkDel(bridge))
		}
	}
	return result, nil
}

func (b *bridgeNetwork) attach(n guestwire.Network, id, ifname string, slot int, ns netns.NsHandle) (netlink.Link, error) {
	gateway, gateway6 := bridgeGateways(n)
	if b.gateway != gateway || b.gateway6 != gateway6 ||
		b.internal != slices.Contains(n.Options, guestwire.NetworkInternal) {
		return nil, fmt.Errorf("bridge configuration changed")
	}
	attrs := netlink.NewLinkAttrs()
	attrs.Name = "kv" + id[:12]
	if slot != 0 {
		attrs.Name = fmt.Sprintf("kv%s%02x", id[:11], slot)
	}
	peer := netlink.NewVeth(attrs)
	peer.PeerName = ifname
	// The bridge and peers may still have this IP cached after a restart.
	address := netip.AddrFrom4(n.IP)
	if !gateway.IsValid() {
		prefix, _, _ := n.IPv6()
		address = prefix.Addr()
	}
	mac, err := n.MAC()
	if err != nil {
		return nil, err
	}
	if mac == nil {
		mac = addressMAC(address, 0x42)
	}
	peer.PeerHardwareAddr = mac
	peer.PeerNamespace = netlink.NsFd(ns)
	if err := netlink.LinkAdd(peer); err != nil {
		return nil, fmt.Errorf("create veth: %w", err)
	}
	if err := netlink.LinkSetMaster(peer, b.link); err != nil {
		return nil, errors.Join(err, netlink.LinkDel(peer))
	}
	if err := netlink.LinkSetUp(peer); err != nil {
		return nil, errors.Join(err, netlink.LinkDel(peer))
	}
	return peer, nil
}

func ipNetwork(addr netip.Addr, prefix int) *net.IPNet {
	return &net.IPNet{IP: addr.AsSlice(), Mask: net.CIDRMask(prefix, addr.BitLen())}
}

func upstreamResolvers() []netip.Addr {
	var nameservers []netip.Addr
	for _, file := range []string{"/etc/resolv.conf", "/run/systemd/resolve/resolv.conf"} {
		data, err := os.ReadFile(file)
		if err != nil {
			continue
		}
		for line := range strings.SplitSeq(string(data), "\n") {
			f := strings.Fields(line)
			if len(f) < 2 || f[0] != "nameserver" {
				continue
			}
			addr, err := netip.ParseAddr(f[1])
			if err == nil && addr.IsGlobalUnicast() {
				nameservers = append(nameservers, addr)
			}
		}
		if len(nameservers) != 0 {
			break
		}
	}
	return nameservers
}

func (b *bridgeNetwork) close() error {
	err := netlink.LinkDel(b.link)
	var missing netlink.LinkNotFoundError
	if err != nil && !errors.As(err, &missing) && !errors.Is(err, unix.ENODEV) {
		return err
	}
	if b.dns != nil {
		b.dns.close()
	}
	b.stopForwarding()
	return nil
}

func (r *Runtime) removeBridge(name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, child := range r.children {
		if child.network != nil && child.network.usesBridge(name) {
			return fmt.Errorf("bridge %s has a running endpoint", name)
		}
	}
	if bridge := r.bridges[name]; bridge != nil {
		if err := bridge.close(); err != nil {
			return err
		}
		delete(r.bridges, name)
	}
	return nil
}
