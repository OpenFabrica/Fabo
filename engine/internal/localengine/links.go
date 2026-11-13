//go:build linux || darwin

package localengine

import (
	"fmt"
	"net/netip"
	"slices"
	"strings"

	"github.com/libracoder/kiri/engine/internal/guestwire"
)

// containerLink retains the target ID across renames and restarts.
type containerLink struct {
	ID, Name, Alias string
	NetworkID       string `json:",omitempty"`
}

// resolveLinks is called with e.mu held.
func (e *Engine) resolveLinks(values []string, networkID string) ([]containerLink, error) {
	var links []containerLink
	for _, value := range values {
		name, alias, found := strings.Cut(value, ":")
		if !found {
			alias = name
		}
		name = strings.TrimLeft(name, "/")
		if last := strings.LastIndexByte(alias, '/'); last >= 0 {
			alias = alias[last+1:]
		}
		if !containerNamePattern.MatchString(name) || !containerNamePattern.MatchString(alias) {
			return nil, fmt.Errorf("%w: invalid link: %s", errInvalid, value)
		}
		target, err := e.find(name)
		if err != nil {
			return nil, fmt.Errorf("%w: could not get container for %s", errMissing, name)
		}
		links = append(links, containerLink{ID: target.id, Name: target.name, Alias: alias, NetworkID: networkID})
	}
	return links, nil
}

func (c *container) inspectLinks(networkID string) []string {
	var result []string
	for _, link := range c.links {
		if link.NetworkID == networkID {
			result = append(result, fmt.Sprintf("/%s:/%s/%s", link.Name, c.name, link.Alias))
		}
	}
	return result
}

// linkWouldCycle checks whether a new link on an existing container would
// make it wait for itself at startup. The caller holds e.mu.
func (e *Engine) linkWouldCycle(source, target string) bool {
	pending := []string{target}
	seen := make(map[string]bool)
	for len(pending) != 0 {
		id := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		if id == source {
			return true
		}
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		if container := e.containers[id]; container != nil {
			pending = append(pending, container.networkTarget())
			for _, link := range container.links {
				pending = append(pending, link.ID)
			}
		}
	}
	return false
}

// linkHosts resolves each target's current address on the selected bridge.
// The generated entries belong to this run and are never saved as ExtraHosts.
// The caller holds e.mu.
func (e *Engine) linkHosts(c *container) ([]string, error) {
	var hosts []string
	for _, link := range c.links {
		networkID := link.NetworkID
		if networkID == "" {
			networkID = c.networkID
		}
		target := e.containers[link.ID]
		if target == nil {
			return nil, fmt.Errorf("%w: could not get container for %s", errMissing, link.Name)
		}
		if target.state != "running" {
			return nil, fmt.Errorf("%w: Cannot link to a non running container: /%s AS /%s/%s", errConflict, target.name, c.name, link.Alias)
		}
		var address netip.Addr
		for _, endpoint := range target.endpoints() {
			if endpoint.ID != networkID || endpoint.Network.Bridge == "" {
				continue
			}
			if networkID != "bridge" && slices.Contains(endpoint.Network.DNSNames, link.Alias) {
				break
			}
			if endpoint.Network.IP != [4]byte{} {
				address = netip.AddrFrom4(endpoint.Network.IP)
			} else if !guestwire.IPv6Disabled(target.run.Sysctls, endpoint.Interface) {
				ip, _, _ := e.liveNetwork(endpoint.Network).IPv6()
				address = ip.Addr()
			}
			break
		}
		if !address.IsValid() {
			if networkID == "bridge" {
				return nil, fmt.Errorf("%w: Cannot link to /%s, as it does not belong to the default network", errInvalid, target.name)
			}
			continue
		}
		names := []string{link.Alias}
		if host := target.run.Hostname; containerNamePattern.MatchString(host) && !slices.Contains(names, host) {
			names = append(names, host)
		}
		if containerNamePattern.MatchString(target.name) && !slices.Contains(names, target.name) {
			names = append(names, target.name)
		}
		hosts = append(hosts, address.String()+"\t"+strings.Join(names, " "))
	}
	return hosts, nil
}
