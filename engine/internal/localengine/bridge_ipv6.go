//go:build linux || darwin

package localengine

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/netip"
	"os"
	"path/filepath"
	"slices"

	"github.com/OpenFabrica/FabriBay/engine/internal/guestwire"
)

const bridgeIPv6File = "bridge-ipv6.json"

func (e *Engine) bridgeNetwork() dockerNetwork {
	n := bridgeNetwork()
	if e.bridgeIPv6Enabled {
		n.IPv6 = e.bridgeIPv6
	}
	return n
}

func (e *Engine) freeIPv6Subnet(subnet netip.Prefix) bool {
	return (e.bridgeIPv6 == nil || !e.bridgeIPv6.Subnet.Overlaps(subnet)) && freeSubnet6(subnet, e.networks)
}

// liveNetwork hides dormant default bridge reservations while IPv6 is off.
// The saved endpoints still reserve their addresses for a future boot.
func (e *Engine) liveNetwork(n guestwire.Network) guestwire.Network {
	if n.Bridge == guestwire.DefaultBridge && !e.bridgeIPv6Enabled {
		return n.WithoutIPv6()
	}
	return n
}

func (e *Engine) saveBridgeIPv6(subnet netip.Prefix) error {
	pool := &ipv6Pool{Subnet: subnet, Gateway: subnet.Addr().Next()}
	if subnet.Bits() != 64 || !pool.valid() || !freeSubnet6(subnet, e.networks) {
		return fmt.Errorf("invalid or overlapping default bridge IPv6 subnet %s", subnet)
	}
	if err := saveJSON(e.state.dir, bridgeIPv6File, pool); err != nil {
		return fmt.Errorf("save default bridge IPv6 subnet: %w", err)
	}
	e.bridgeIPv6 = pool
	return nil
}

func (e *Engine) restoreBridgeIPv6() error {
	raw, err := os.ReadFile(filepath.Join(e.state.dir, bridgeIPv6File))
	switch {
	case err == nil:
		var pool ipv6Pool
		if err := json.Unmarshal(raw, &pool); err != nil {
			return fmt.Errorf("read default bridge IPv6 subnet: %w", err)
		}
		if pool.Subnet.Bits() != 64 || !pool.valid() || !freeSubnet6(pool.Subnet, e.networks) {
			return fmt.Errorf("invalid saved default bridge IPv6 subnet %s", pool.Subnet)
		}
		e.bridgeIPv6 = &pool
	case !errors.Is(err, os.ErrNotExist):
		return fmt.Errorf("read default bridge IPv6 subnet: %w", err)
	}
	e.bridgeIPv6Enabled = bridgeIPv6Requested()
	if !e.bridgeIPv6Enabled {
		return nil
	}
	if e.bridgeIPv6 == nil {
		if err := e.saveBridgeIPv6(e.automaticSubnet6("bridge")); err != nil {
			return err
		}
	}
	bridge := e.bridgeNetwork()
	for _, id := range slices.Sorted(maps.Keys(e.containers)) {
		c := e.containers[id]
		endpoints := c.endpoints()
		for i, ep := range endpoints {
			if ep.Network.Bridge != guestwire.DefaultBridge {
				continue
			}
			ip, _, err := ep.Network.IPv6()
			if err != nil || ip.IsValid() && !bridge.matchesEndpoint(ep.Network) {
				return fmt.Errorf("container %s references an invalid default bridge IPv6 endpoint", id)
			}
			if !ip.IsValid() {
				ip, err := e.allocateIPv6(bridge, netip.Addr{})
				if err != nil {
					return err
				}
				endpoints[i].Network.SetIPv6(netip.PrefixFrom(ip, bridge.IPv6.Subnet.Bits()), bridge.IPv6.Gateway)
				c.setEndpoints(endpoints)
				if err := e.save(c); err != nil {
					return fmt.Errorf("save default bridge address for container %s: %w", id, err)
				}
			}
			break
		}
	}
	return nil
}

func bridgeIPv6Requested() bool {
	value := os.Getenv("FABO_BRIDGE_IPV6")
	return value == "1" || value == "true"
}

// Rust derives the default bridge's /64 from the original state directory
// and does not write it into networks.json. The saved fixed endpoints reveal
// that subnet even when importing a snapshot from a different location.
func (e *Engine) importBridgeIPv6(source string, records []migrationContainer) error {
	var subnet netip.Prefix
	for _, record := range records {
		for name, config := range record.endpoints {
			if (name != "bridge" && name != "default" && name != "") || !config.ip6.IsValid() {
				continue
			}
			want := netip.PrefixFrom(config.ip6, 64).Masked()
			if subnet.IsValid() && subnet != want {
				return fmt.Errorf("fixed IPv6 addresses on the source default bridge span multiple subnets")
			}
			subnet = want
		}
	}
	if !subnet.IsValid() {
		if !bridgeIPv6Requested() {
			return nil
		}
		subnet = e.automaticSubnet6From(source, "bridge")
	}
	if err := e.saveBridgeIPv6(subnet); err != nil {
		return err
	}
	e.bridgeIPv6Enabled = true
	return nil
}
