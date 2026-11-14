package guestagent

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"

	"github.com/OpenFabrica/FabriBay/engine/internal/guestwire"
)

var loopbackResolver = netip.MustParseAddr("127.0.0.11")

type endpoint struct {
	spec   guestwire.Network
	bridge *bridgeNetwork
	veth   netlink.Link
}

type containerNetwork struct {
	mu               sync.RWMutex
	ns               netns.NsHandle
	handle           *netlink.Handle
	endpoints        map[string]endpoint
	resolver         *bridgeDNS
	vm               bool
	sysctls          map[string]string
	closed           bool
	closeProxy       func()
	kubernetesRoutes []netlink.Route
}

func openContainerNetwork(pid int, vm bool, spec guestwire.RunSpec) (*containerNetwork, error) {
	ns, err := netns.GetFromPid(pid)
	if err != nil {
		return nil, err
	}
	handle, err := netlink.NewHandleAt(ns, unix.NETLINK_ROUTE)
	if err != nil {
		_ = ns.Close()
		return nil, err
	}
	n := &containerNetwork{ns: ns, handle: handle, endpoints: make(map[string]endpoint), vm: vm, sysctls: spec.Sysctls}
	lo, err := handle.LinkByName("lo")
	if err == nil {
		err = handle.LinkSetUp(lo)
	}
	if err == nil && containerDNSAddress(spec, vm) == loopbackResolver {
		n.resolver, err = listenContainerDNS(ns, n.lookup, spec.DNS.Servers)
	}
	if err != nil {
		n.close()
		return nil, err
	}
	return n, nil
}

func inNetworkNamespace(ns netns.NsHandle, work func() error) error {
	result := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		original, err := netns.Get()
		if err != nil {
			runtime.UnlockOSThread()
			result <- err
			return
		}
		defer original.Close()
		if err := netns.Set(ns); err != nil {
			runtime.UnlockOSThread()
			result <- err
			return
		}
		err = work()
		if restoreErr := netns.Set(original); restoreErr == nil {
			runtime.UnlockOSThread()
		} else {
			err = errors.Join(err, restoreErr)
		}
		result <- err
	}()
	return <-result
}

func listenContainerDNS(ns netns.NsHandle, lookup func(string) ([]netip.Addr, bool), servers []netip.Addr) (*bridgeDNS, error) {
	// Bind in the container namespace and start workers in the guest.
	var udp *net.UDPConn
	var tcp *net.TCPListener
	err := inNetworkNamespace(ns, func() error {
		ip := loopbackResolver.AsSlice()
		var err error
		udp, err = net.ListenUDP("udp4", &net.UDPAddr{IP: ip, Port: 53})
		if err != nil {
			return err
		}
		tcp, err = net.ListenTCP("tcp4", &net.TCPAddr{IP: ip, Port: 53})
		return err
	})
	if err != nil {
		if udp != nil {
			_ = udp.Close()
		}
		if tcp != nil {
			_ = tcp.Close()
		}
		return nil, err
	}
	var upstream dnsUpstream
	if len(servers) != 0 {
		upstream = dnsUpstream{servers: servers, dial: func(ctx context.Context, protocol, address string) (net.Conn, error) {
			var conn net.Conn
			err := inNetworkNamespace(ns, func() error {
				var err error
				conn, err = (&net.Dialer{}).DialContext(ctx, protocol, address)
				return err
			})
			if err != nil && conn != nil {
				_ = conn.Close()
			}
			return conn, err
		}}
	}
	return newBridgeDNS(udp, tcp, netip.MustParsePrefix("127.0.0.0/8"), false, false, lookup, upstream), nil
}

func (n *containerNetwork) lookup(name string) ([]netip.Addr, bool) {
	n.mu.RLock()
	defer n.mu.RUnlock()
	internal := true
	var addresses []netip.Addr
	for _, ep := range n.endpoints {
		internal = internal && ep.bridge.internal
		if ep.bridge.dns != nil {
			addresses = append(addresses, ep.bridge.dns.addresses(name)...)
		}
	}
	if n.vm && !internal && isHostRelayName(name) {
		return []netip.Addr{netip.MustParseAddr(guestwire.HostRelayAddress)}, false
	}
	return addresses, internal
}

func endpointSlot(ifname string) (int, error) {
	digits, ok := strings.CutPrefix(ifname, "eth")
	slot, err := strconv.Atoi(digits)
	if !ok || err != nil || slot < 0 || slot >= guestwire.MaxEndpoints || digits != strconv.Itoa(slot) {
		return 0, fmt.Errorf("invalid network interface %q", ifname)
	}
	return slot, nil
}

func (n *containerNetwork) attach(id, ifname string, spec guestwire.Network, bridge *bridgeNetwork) error {
	slot, err := endpointSlot(ifname)
	if err != nil {
		return err
	}
	priority, err := spec.GatewayPriority()
	if err != nil {
		return err
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.closed || n.endpoints[ifname].veth != nil {
		return fmt.Errorf("network namespace or interface is unavailable")
	}
	for _, ep := range n.endpoints {
		if ep.spec.Bridge == spec.Bridge {
			return fmt.Errorf("container already has an endpoint on bridge %s", spec.Bridge)
		}
	}
	veth, err := bridge.attach(spec, id, ifname, slot, n.ns)
	if err != nil {
		return err
	}
	ip6, gateway6, _ := spec.IPv6()
	useIPv6 := ip6.IsValid() && !guestwire.IPv6Disabled(n.sysctls, ifname)
	var addresses []netip.Prefix
	var gateways []netip.Addr
	if !slices.Contains(spec.Options, guestwire.NetworkNoIPv4) {
		addresses = append(addresses, netip.PrefixFrom(netip.AddrFrom4(spec.IP), int(spec.Prefix)))
		gateways = append(gateways, netip.AddrFrom4(spec.Gateway))
	}
	if useIPv6 {
		addresses = append(addresses, ip6)
		gateways = append(gateways, gateway6)
	}
	configure := func() error {
		if err := inNetworkNamespace(n.ns, func() error {
			path := "/proc/sys/net/ipv6/conf/" + ifname + "/"
			value := "1"
			if useIPv6 {
				value = "0"
			}
			err := os.WriteFile(path+"disable_ipv6", []byte(value), 0)
			if !useIPv6 && errors.Is(err, os.ErrNotExist) {
				return nil
			}
			if err == nil && useIPv6 {
				err = os.WriteFile(path+"accept_ra", []byte("0"), 0)
			}
			return err
		}); err != nil {
			return err
		}
		eth, err := n.handle.LinkByName(ifname)
		if err != nil {
			return err
		}
		for _, address := range addresses {
			info := &netlink.Addr{IPNet: ipNetwork(address.Addr(), address.Bits())}
			if address.Addr().Is6() {
				info.Flags = unix.IFA_F_NODAD
			}
			if err := n.handle.AddrAdd(eth, info); err != nil {
				return err
			}
		}
		if err := n.handle.LinkSetUp(eth); err != nil {
			return err
		}
		if bridge.internal {
			return nil
		}
		// Each family can prefer a different external endpoint.
		metric := (32767-int(priority))*guestwire.MaxEndpoints + slot + 1
		for _, gateway := range gateways {
			if err := n.handle.RouteAdd(&netlink.Route{LinkIndex: eth.Attrs().Index, Gw: gateway.AsSlice(), Priority: metric}); err != nil {
				return err
			}
		}
		return nil
	}
	if err := configure(); err != nil {
		return errors.Join(err, deleteVeth(veth))
	}
	n.endpoints[ifname] = endpoint{spec, bridge, veth}
	if bridge.dns != nil {
		for _, addr := range addresses {
			bridge.dns.register(spec.DNSNames, addr.Addr())
		}
	}
	return nil
}

func (n *containerNetwork) detach(ifname string, spec guestwire.Network) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	ep, ok := n.endpoints[ifname]
	if !ok || ep.spec.Bridge != spec.Bridge || ep.spec.IP != spec.IP {
		return fmt.Errorf("endpoint does not exist")
	}
	if err := deleteVeth(ep.veth); err != nil {
		return err
	}
	n.forget(ifname, ep)
	return nil
}

func (n *containerNetwork) forget(ifname string, ep endpoint) {
	if ep.bridge.dns != nil {
		if !slices.Contains(ep.spec.Options, guestwire.NetworkNoIPv4) {
			ep.bridge.dns.unregister(ep.spec.DNSNames, netip.AddrFrom4(ep.spec.IP))
		}
		if ip, _, _ := ep.spec.IPv6(); ip.IsValid() && !guestwire.IPv6Disabled(n.sysctls, ifname) {
			ep.bridge.dns.unregister(ep.spec.DNSNames, ip.Addr())
		}
	}
	delete(n.endpoints, ifname)
}

func (n *containerNetwork) close() {
	if n.closeProxy != nil {
		n.closeProxy()
	}
	n.mu.Lock()
	n.closed = true
	if err := n.removeKubernetesRoutes(); err != nil {
		fmt.Fprintln(os.Stderr, err)
	}
	for ifname, ep := range n.endpoints {
		retireVeth(ep.veth)
		n.forget(ifname, ep)
	}
	n.mu.Unlock()
	if n.resolver != nil {
		n.resolver.close()
	}
	n.handle.Close()
	_ = n.ns.Close()
}

func (n *containerNetwork) usesBridge(name string) bool {
	n.mu.RLock()
	defer n.mu.RUnlock()
	for _, ep := range n.endpoints {
		if ep.spec.Bridge == name {
			return true
		}
	}
	return false
}

func (r *Runtime) changeNetwork(id, ifname string, spec guestwire.Network, attach bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	child := r.children[id]
	if child == nil || child.network == nil {
		return fmt.Errorf("container is not running")
	}
	if !attach {
		return child.network.detach(ifname, spec)
	}
	return r.attachNetwork(child.network, id, ifname, spec)
}

func (r *Runtime) attachNetwork(n *containerNetwork, id, ifname string, spec guestwire.Network) error {
	if err := validateNetwork(spec); err != nil {
		return err
	}
	if spec.Bridge == "" {
		return fmt.Errorf("an endpoint requires a bridge")
	}
	bridge := r.bridges[spec.Bridge]
	if bridge == nil {
		var err error
		bridge, err = r.openBridge(spec)
		if err != nil {
			return fmt.Errorf("set up bridge %s: %w", spec.Bridge, err)
		}
		r.bridges[spec.Bridge] = bridge
	}
	return n.attach(id, ifname, spec, bridge)
}
