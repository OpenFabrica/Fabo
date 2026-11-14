//go:build linux || darwin

package localengine

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"net"
	"net/netip"
	"slices"
	"strings"

	"github.com/OpenFabrica/FabriBay/engine/internal/guestwire"
)

func validNetworkAlias(alias string) bool {
	name := strings.TrimSuffix(alias, ".")
	if len(name) == 0 || len(name) > 253 {
		return false
	}
	for label := range strings.SplitSeq(name, ".") {
		if len(label) == 0 || len(label) > 63 {
			return false
		}
		for _, ch := range label {
			if !(ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || ch == '-' || ch == '_') {
				return false
			}
		}
	}
	return true
}

type endpointConfig struct {
	ip, ip6   netip.Addr
	mac       net.HardwareAddr
	aliases   []string
	links     []string
	priority  int16
	networkID string
}

func parseDNSConfig(host map[string]json.RawMessage) (guestwire.DNSConfig, error) {
	var dns guestwire.DNSConfig
	for _, field := range []struct {
		name string
		dest any
	}{{"Dns", &dns.Servers}, {"DnsSearch", &dns.Search}, {"DnsOptions", &dns.Options}} {
		if value := host[field.name]; len(value) != 0 {
			if err := json.Unmarshal(value, field.dest); err != nil {
				return guestwire.DNSConfig{}, fmt.Errorf("%w: invalid %s list", errInvalid, field.name)
			}
			delete(host, field.name)
		}
	}
	if err := dns.Validate(); err != nil {
		return guestwire.DNSConfig{}, fmt.Errorf("%w: %v", errInvalid, err)
	}
	return dns, nil
}

func parseExtraHosts(raw json.RawMessage) ([]string, error) {
	var hosts []string
	if len(raw) == 0 {
		return nil, nil
	}
	if err := json.Unmarshal(raw, &hosts); err != nil {
		return nil, fmt.Errorf("%w: invalid ExtraHosts list", errInvalid)
	}
	for i, entry := range hosts {
		separator := strings.IndexAny(entry, ":=")
		if separator < 1 {
			return nil, fmt.Errorf("%w: extra host must use hostname:address", errInvalid)
		}
		name, address := entry[:separator], entry[separator+1:]
		if strings.TrimSpace(name) != name || strings.TrimSpace(address) != address {
			return nil, fmt.Errorf("%w: unexpected whitespace in extra host", errInvalid)
		}
		address, name, err := guestwire.ParseExtraHost(address + "\t" + name)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", errInvalid, err)
		}
		hosts[i] = address + "\t" + name
	}
	return hosts, nil
}

func parseEndpoint(fields map[string]json.RawMessage) (endpointConfig, error) {
	var config endpointConfig
	for field, raw := range fields {
		switch string(bytes.TrimSpace(raw)) {
		case "null", "false", "0", `""`, "[]", "{}":
			continue
		}
		switch field {
		case "Aliases":
			if err := json.Unmarshal(raw, &config.aliases); err != nil {
				return endpointConfig{}, fmt.Errorf("%w: invalid network aliases", errInvalid)
			}
		case "Links":
			if err := json.Unmarshal(raw, &config.links); err != nil {
				return endpointConfig{}, fmt.Errorf("%w: invalid endpoint Links list", errInvalid)
			}
			for _, name := range config.aliases {
				if !validNetworkAlias(name) {
					return endpointConfig{}, fmt.Errorf("%w: invalid network alias %q", errInvalid, name)
				}
			}
		case "IPAMConfig":
			var ipam struct {
				IPv4Address, IPv6Address string
				LinkLocalIPs             []string
			}
			if err := json.Unmarshal(raw, &ipam); err != nil || len(ipam.LinkLocalIPs) != 0 {
				return endpointConfig{}, fmt.Errorf("%w: unsupported endpoint IPAM configuration", errInvalid)
			}
			if ipam.IPv4Address != "" {
				ip, err := netip.ParseAddr(ipam.IPv4Address)
				if err != nil || !ip.Is4() {
					return endpointConfig{}, fmt.Errorf("%w: invalid endpoint IPv4 address", errInvalid)
				}
				config.ip = ip
			}
			if ipam.IPv6Address != "" {
				ip, err := netip.ParseAddr(ipam.IPv6Address)
				if err != nil || !ip.Is6() || ip.Is4In6() || ip.Zone() != "" {
					return endpointConfig{}, fmt.Errorf("%w: invalid endpoint IPv6 address", errInvalid)
				}
				config.ip6 = ip
			}
		case "MacAddress":
			var value string
			if err := json.Unmarshal(raw, &value); err != nil {
				return endpointConfig{}, fmt.Errorf("%w: invalid MAC address", errInvalid)
			}
			mac, err := guestwire.ParseEndpointMAC(value)
			if err != nil {
				return endpointConfig{}, fmt.Errorf("%w: %v", errInvalid, err)
			}
			config.mac = mac
		case "GwPriority":
			if err := json.Unmarshal(raw, &config.priority); err != nil {
				return endpointConfig{}, fmt.Errorf("%w: invalid gateway priority", errInvalid)
			}
		case "NetworkID":
			if err := json.Unmarshal(raw, &config.networkID); err != nil {
				return endpointConfig{}, fmt.Errorf("%w: invalid network ID", errInvalid)
			}
		default:
			return endpointConfig{}, fmt.Errorf("%w: unsupported network endpoint option %q", errInvalid, field)
		}
	}
	return config, nil
}

func configureEndpoint(config *Config, mode string, endpoints map[string]map[string]json.RawMessage) error {
	if mode == "" || mode == "default" {
		mode = "bridge"
		_, bridge := endpoints["bridge"]
		_, defaultBridge := endpoints["default"]
		if len(endpoints) != 0 && !bridge && !defaultBridge {
			mode = slices.Sorted(maps.Keys(endpoints))[0]
		}
	}
	if target, join := strings.CutPrefix(mode, containerNetworkPrefix); join {
		if target == "" {
			return fmt.Errorf("%w: shared network requires a container name or ID", errInvalid)
		}
		for name, fields := range endpoints {
			if name != mode || len(fields) != 0 {
				return fmt.Errorf("%w: a shared container network cannot have its own endpoints", errInvalid)
			}
		}
		config.network = mode
		return nil
	}
	if len(endpoints) > guestwire.MaxEndpoints {
		return fmt.Errorf("%w: too many networks", errInvalid)
	}
	config.network, config.bridge = mode, mode != "none" && mode != "host"
	config.networkEndpoints = make(map[string]endpointConfig, len(endpoints))
	for name, fields := range endpoints {
		if name == "default" {
			name = "bridge"
		}
		parsed, err := parseEndpoint(fields)
		if err != nil {
			return err
		}
		if _, duplicate := config.networkEndpoints[name]; duplicate {
			return fmt.Errorf("%w: duplicate endpoint %q", errInvalid, name)
		}
		config.networkEndpoints[name] = parsed
	}
	return nil
}
