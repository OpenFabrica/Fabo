package guestagent

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"

	"github.com/libracoder/kiri/engine/internal/guestwire"
)

func (r *Runtime) routeKubernetes(id string) error {
	r.mu.Lock()
	node := r.children[id]
	r.mu.Unlock()
	if node == nil || node.network == nil {
		return fmt.Errorf("Kubernetes node is not running on a bridge")
	}
	network := node.network
	network.mu.Lock()
	defer network.mu.Unlock()
	ep, ok := network.endpoints["eth0"]
	if network.closed || !ok || ep.spec.IP == [4]byte{} {
		return fmt.Errorf("Kubernetes node has no IPv4 endpoint")
	}
	ranges := guestwire.KubernetesRanges()
	if len(network.kubernetesRoutes) == len(ranges) {
		return nil
	}
	if err := network.removeKubernetesRoutes(); err != nil {
		return err
	}
	via := netip.AddrFrom4(ep.spec.IP)
	for _, prefix := range ranges {
		route := netlink.Route{
			LinkIndex: ep.bridge.link.Attrs().Index, Protocol: unix.RTPROT_STATIC,
			Dst: &net.IPNet{IP: prefix.Addr().AsSlice(), Mask: net.CIDRMask(prefix.Bits(), 32)}, Gw: via.AsSlice(),
		}
		if err := netlink.RouteAdd(&route); err != nil {
			return errors.Join(fmt.Errorf("route Kubernetes range %s: %w", prefix, err), network.removeKubernetesRoutes())
		}
		network.kubernetesRoutes = append(network.kubernetesRoutes, route)
	}
	return nil
}

// Only routes successfully added by this run are eligible for removal.
func (n *containerNetwork) removeKubernetesRoutes() error {
	var failed []netlink.Route
	var failure error
	for _, route := range n.kubernetesRoutes {
		if err := netlink.RouteDel(&route); err != nil && !errors.Is(err, unix.ESRCH) && !errors.Is(err, os.ErrNotExist) {
			failed = append(failed, route)
			failure = errors.Join(failure, fmt.Errorf("remove Kubernetes route %s: %w", route.Dst, err))
		}
	}
	n.kubernetesRoutes = failed
	return failure
}
