//go:build linux || darwin

package localengine

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/netip"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/libracoder/kiri/engine/internal/guestwire"
)

var networkNamePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$`)

func (n dockerNetwork) api() map[string]any {
	driver := n.driver()
	config := []map[string]any{}
	if n.Subnet.IsValid() {
		ipv4 := map[string]any{"Subnet": n.Subnet.String(), "Gateway": n.Gateway.String()}
		if n.IPRange.IsValid() {
			ipv4["IPRange"] = n.IPRange.String()
		}
		if len(n.Auxiliary) != 0 {
			ipv4["AuxiliaryAddresses"] = n.Auxiliary
		}
		config = append(config, ipv4)
	}
	if n.IPv6 != nil {
		config = append(config, n.IPv6.api())
	}
	options := n.Options
	if options == nil {
		options = map[string]string{}
	}
	return map[string]any{
		"Id": n.ID, "Name": n.Name, "Driver": driver, "Scope": "local", "Internal": n.Internal,
		"Created": n.Created, "Labels": n.Labels, "Attachable": n.Attachable, "EnableIPv4": n.Subnet.IsValid(), "EnableIPv6": n.IPv6 != nil,
		"Options": options, "IPAM": map[string]any{"Driver": "default", "Options": n.IPAMOptions, "Config": config},
	}
}

func (e *Engine) networksHTTP(w http.ResponseWriter, r *http.Request) {
	filters, err := parseFilters(r.URL.Query().Get("filters"), "name", "id", "driver", "label", "scope", "type")
	if err != nil {
		writeError(w, err)
		return
	}
	e.mu.Lock()
	entries := []dockerNetwork{e.bridgeNetwork(), hostNetwork(), noneNetwork()}
	entries = append(entries, slices.Collect(maps.Values(e.networks))...)
	e.mu.Unlock()
	slices.SortFunc(entries, func(a, b dockerNetwork) int { return strings.Compare(a.Name, b.Name) })
	rows := []map[string]any{}
	for _, n := range entries {
		if n.matches(filters) {
			rows = append(rows, n.api())
		}
	}
	writeJSON(w, http.StatusOK, rows)
}

func (e *Engine) inspectNetworkHTTP(w http.ResponseWriter, r *http.Request) {
	e.mu.Lock()
	n, err := e.network(r.PathValue("id"))
	if err != nil {
		e.mu.Unlock()
		writeError(w, err)
		return
	}
	data := n.api()
	endpoints := make(map[string]any)
	for _, c := range e.containers {
		if c.state != "running" {
			continue
		}
		for _, ep := range c.endpoints() {
			if ep.ID != n.ID {
				continue
			}
			var address, address6 string
			if ep.Network.Prefix != 0 {
				address = netip.PrefixFrom(netip.AddrFrom4(ep.Network.IP), int(ep.Network.Prefix)).String()
			}
			if !guestwire.IPv6Disabled(c.run.Sysctls, ep.Interface) {
				if ip, _, _ := e.liveNetwork(ep.Network).IPv6(); ip.IsValid() {
					address6 = ip.String()
				}
			}
			endpoints[c.id] = map[string]any{"Name": c.name, "EndpointID": c.id, "IPv4Address": address, "IPv6Address": address6}
		}
	}
	e.mu.Unlock()
	data["Containers"] = endpoints
	writeJSON(w, http.StatusOK, data)
}

type networkIPAM struct {
	Driver  string
	Options map[string]string
	Config  []ipamPoolConfig
}

type ipamPoolConfig struct {
	Subnet, Gateway, IPRange string
	AuxAddresses             map[string]string `json:"AuxiliaryAddresses"`
}

func (e *Engine) createNetworkHTTP(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name, Driver, Scope                                   string
		Internal, Attachable, EnableIPv6, Ingress, ConfigOnly bool
		EnableIPv4                                            *bool
		IPAM                                                  networkIPAM
		Options, Labels                                       map[string]string
		ConfigFrom                                            struct{ Network string }
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
		writeError(w, fmt.Errorf("%w: %v", errInvalid, err))
		return
	}
	switch body.Name {
	case "bridge", "default", "none", "host":
		writeError(w, fmt.Errorf("%w: network %q is reserved", errConflict, body.Name))
		return
	}
	if !networkNamePattern.MatchString(body.Name) {
		writeError(w, fmt.Errorf("%w: invalid network name", errInvalid))
		return
	}
	if (body.Driver != "" && body.Driver != "bridge") || (body.Scope != "" && body.Scope != "local") ||
		body.Ingress || body.ConfigOnly || body.ConfigFrom.Network != "" {
		writeError(w, fmt.Errorf("%w: only local bridge networks are supported", errInvalid))
		return
	}
	if (body.IPAM.Driver != "" && body.IPAM.Driver != "default") || len(body.IPAM.Config) > 2 {
		writeError(w, fmt.Errorf("%w: unsupported network IPAM configuration", errInvalid))
		return
	}
	n := dockerNetwork{
		Name: body.Name, Internal: body.Internal, Labels: body.Labels, Attachable: body.Attachable, Created: time.Now().UTC(),
		Options: body.Options, IPAMOptions: body.IPAM.Options,
	}
	n.NoIPv4 = body.EnableIPv4 != nil && !*body.EnableIPv4
	if n.NoIPv4 && !body.EnableIPv6 {
		writeError(w, fmt.Errorf("%w: IPv4 or IPv6 must be enabled", errInvalid))
		return
	}
	for _, config := range body.IPAM.Config {
		if config.Subnet == "" && config.Gateway == "" && config.IPRange == "" && len(config.AuxAddresses) == 0 {
			continue
		}
		subnet, err := netip.ParsePrefix(config.Subnet)
		if err != nil || subnet != subnet.Masked() {
			writeError(w, fmt.Errorf("%w: invalid network subnet %q", errInvalid, config.Subnet))
			return
		}
		if subnet.Addr().Is6() && !subnet.Addr().Is4In6() {
			if !body.EnableIPv6 || n.IPv6 != nil {
				writeError(w, fmt.Errorf("%w: IPv6 must be enabled and can have only one subnet", errInvalid))
				return
			}
			n.IPv6, err = parseIPv6Pool(config, subnet)
			if err != nil {
				writeError(w, err)
				return
			}
			continue
		}
		if n.NoIPv4 || n.Subnet.IsValid() || !subnet.Addr().Is4() {
			writeError(w, fmt.Errorf("%w: IPv4 must be enabled and can have only one subnet", errInvalid))
			return
		}
		n.Subnet, n.Gateway = subnet, subnet.Addr().Next()
		if config.Gateway != "" {
			n.Gateway, err = netip.ParseAddr(config.Gateway)
		}
		if err != nil || !usableAddress(n.Subnet, n.Gateway) {
			writeError(w, fmt.Errorf("%w: no matching subnet for gateway %s", errInvalid, config.Gateway))
			return
		}
		if config.IPRange != "" {
			n.IPRange, err = netip.ParsePrefix(config.IPRange)
			if err != nil {
				writeError(w, fmt.Errorf("%w: invalid IPv4 IPRange %q", errInvalid, config.IPRange))
				return
			}
		}
		if len(config.AuxAddresses) != 0 {
			n.Auxiliary = make(map[string]netip.Addr, len(config.AuxAddresses))
			for name, address := range config.AuxAddresses {
				n.Auxiliary[name], err = netip.ParseAddr(address)
				if err != nil {
					writeError(w, fmt.Errorf("%w: invalid auxiliary IPv4 address %q", errInvalid, address))
					return
				}
			}
		}
		if !n.validIPv4Pool() {
			writeError(w, fmt.Errorf("%w: invalid IPv4 subnet, gateway, allocation range or auxiliary address", errInvalid))
			return
		}
	}
	if body.EnableIPv6 && n.IPv6 == nil {
		n.IPv6 = &ipv6Pool{}
	}
	if err := e.createNetwork(&n); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"Id": n.ID, "Warning": ""})
}

func (e *Engine) createNetwork(n *dockerNetwork) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return fmt.Errorf("%w: daemon is shutting down", errConflict)
	}
	if _, exists := e.networks[n.Name]; exists {
		return fmt.Errorf("%w: network %q already exists", errConflict, n.Name)
	}
	if !n.NoIPv4 {
		if n.Subnet.IsValid() {
			if !freeSubnet(n.Subnet, e.networks) {
				return fmt.Errorf("%w: Pool overlaps with other one on this address space: %s", errConflict, n.Subnet)
			}
		} else {
			n.Subnet = availableSubnet(e.networks)
			if !n.Subnet.IsValid() {
				return fmt.Errorf("%w: no automatic network subnets are available", errConflict)
			}
			n.Gateway = n.Subnet.Addr().Next()
		}
	}
	if n.IPv6 != nil {
		if !n.IPv6.Subnet.IsValid() {
			subnet := e.automaticSubnet6(n.Name)
			if !subnet.IsValid() {
				return fmt.Errorf("%w: no automatic IPv6 subnets are available", errConflict)
			}
			n.IPv6.Subnet, n.IPv6.Gateway = subnet, subnet.Addr().Next()
		} else if !e.freeIPv6Subnet(n.IPv6.Subnet) {
			return fmt.Errorf("%w: Pool overlaps with other one on this address space: %s", errConflict, n.IPv6.Subnet)
		}
	}
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return err
	}
	n.ID = hex.EncodeToString(raw[:])
	n.Bridge = "kiri" + n.ID[:10]
	for _, existing := range e.networks {
		if existing.Bridge == n.Bridge {
			return fmt.Errorf("%w: bridge name already reserved", errConflict)
		}
	}
	next := make(map[string]dockerNetwork, len(e.networks)+1)
	maps.Copy(next, e.networks)
	next[n.Name] = *n
	if err := e.saveNetworks(next); err != nil {
		return err
	}
	e.emitNetwork(*n, "create", "")
	return nil
}

func (e *Engine) removeNetworkHTTP(w http.ResponseWriter, r *http.Request) {
	if err := e.removeNetwork(r.Context(), r.PathValue("id")); err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (e *Engine) removeNetwork(ctx context.Context, id string) error {
	e.networkOps.Lock()
	defer e.networkOps.Unlock()
	e.mu.Lock()
	n, err := e.network(id)
	switch {
	case err != nil:
	case e.closed:
		err = fmt.Errorf("%w: daemon is shutting down", errConflict)
	case !guestwire.CustomBridge(n.Bridge):
		err = fmt.Errorf("%w: network %q is predefined", errConflict, n.Name)
	default:
		for _, c := range e.containers {
			if slices.ContainsFunc(c.endpoints(), func(ep containerEndpoint) bool { return ep.ID == n.ID }) {
				err = fmt.Errorf("%w: network %q is used by container %q", errConflict, n.Name, c.name)
				break
			}
		}
	}
	if err != nil {
		e.mu.Unlock()
		return err
	}
	e.mu.Unlock()

	err = e.session.DeleteBridge(ctx, n.Bridge)

	e.mu.Lock()
	if err == nil {
		next := maps.Clone(e.networks)
		delete(next, n.Name)
		err = e.saveNetworks(next)
		if err == nil {
			e.emitNetwork(n, "destroy", "")
		}
	}
	e.mu.Unlock()
	return err
}

func (e *Engine) saveNetworks(next map[string]dockerNetwork) error {
	if e.state != nil {
		if err := saveJSON(e.state.dir, "networks.json", next); err != nil {
			return err
		}
	}
	e.networks = next
	return nil
}

func freeSubnet(subnet netip.Prefix, networks map[string]dockerNetwork) bool {
	if defaultSubnet.Overlaps(subnet) {
		return false
	}
	for _, existing := range networks {
		if existing.Subnet.Overlaps(subnet) {
			return false
		}
	}
	return true
}

func availableSubnet(networks map[string]dockerNetwork) netip.Prefix {
	for i := 18; i <= 31; i++ {
		p := netip.PrefixFrom(netip.AddrFrom4([4]byte{172, byte(i), 0, 0}), 16)
		if freeSubnet(p, networks) {
			return p
		}
	}
	for i := range 256 {
		p := netip.PrefixFrom(netip.AddrFrom4([4]byte{10, 89, byte(i), 0}), 24)
		if freeSubnet(p, networks) {
			return p
		}
	}
	return netip.Prefix{}
}

func (n dockerNetwork) driver() string {
	switch n.Name {
	case "host":
		return "host"
	case "none":
		return "null"
	default:
		return "bridge"
	}
}

func (n dockerNetwork) matches(filters map[string][]string) bool {
	driver, kind := n.driver(), "custom"
	if !guestwire.CustomBridge(n.Bridge) {
		kind = "builtin"
	}
	for key, values := range filters {
		if key == "label" {
			if !matchesLabels(n.Labels, values) {
				return false
			}
			continue
		}
		match := func(value string) bool {
			switch key {
			case "name":
				return strings.Contains(n.Name, value)
			case "id":
				return strings.HasPrefix(n.ID, value)
			case "driver":
				return driver == value
			case "scope":
				return value == "local"
			case "type":
				return kind == value
			}
			return false
		}
		if len(values) > 0 && !slices.ContainsFunc(values, match) {
			return false
		}
	}
	return true
}
