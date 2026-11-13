//go:build linux || darwin

package localengine

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/netip"
	"slices"
	"strconv"
	"strings"
)

type portBinding struct {
	ContainerPort uint16
	Protocol      string
	HostIP        string
	HostPort      uint16
}

type dockerPortBinding struct {
	HostIP   string `json:"HostIp"`
	HostPort string
}

func parsePort(key string) (uint16, string, error) {
	number, proto, ok := strings.Cut(key, "/")
	if !ok {
		proto = "tcp"
	}
	n, err := strconv.ParseUint(number, 10, 16)
	if err != nil || n == 0 || (proto != "tcp" && proto != "udp") {
		return 0, "", fmt.Errorf("%w: invalid TCP or UDP port %q", errInvalid, key)
	}
	return uint16(n), proto, nil
}

func parsePortBindings(raw json.RawMessage) ([]portBinding, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var bindings map[string][]dockerPortBinding
	if err := json.Unmarshal(raw, &bindings); err != nil {
		return nil, fmt.Errorf("%w: invalid port bindings: %v", errInvalid, err)
	}
	var out []portBinding
	for _, key := range slices.Sorted(maps.Keys(bindings)) {
		port, proto, err := parsePort(key)
		if err != nil {
			return nil, err
		}
		for _, b := range bindings[key] {
			var hostPort uint64
			if b.HostPort != "" {
				hostPort, err = strconv.ParseUint(b.HostPort, 10, 16)
				if err != nil {
					return nil, fmt.Errorf("%w: invalid host port %q", errInvalid, b.HostPort)
				}
			}
			hostIP, err := parseBindHost(b.HostIP)
			if err != nil {
				return nil, err
			}
			out = append(out, portBinding{port, proto, hostIP, uint16(hostPort)})
		}
	}
	return out, nil
}

func parseBindHost(host string) (string, error) {
	if host == "" {
		return "", nil
	}
	address := host
	if strings.HasPrefix(address, "[") && strings.HasSuffix(address, "]") {
		address = address[1 : len(address)-1]
	}
	ip, err := netip.ParseAddr(address)
	if err != nil {
		return "", fmt.Errorf("%w: invalid host address %q", errInvalid, host)
	}
	return ip.Unmap().String(), nil
}

func (b portBinding) key() string {
	return fmt.Sprintf("%d/%s", b.ContainerPort, b.Protocol)
}

func mergePorts(image, explicit map[string]struct{}, bindings []portBinding, publishAll bool) (map[string]struct{}, []portBinding) {
	exposed := make(map[string]struct{})
	for _, ports := range []map[string]struct{}{image, explicit} {
		for key := range ports {
			port, proto, err := parsePort(key)
			if err == nil {
				exposed[(portBinding{ContainerPort: port, Protocol: proto}).key()] = struct{}{}
			}
		}
	}
	bound := make(map[string]bool)
	for _, b := range bindings {
		exposed[b.key()] = struct{}{}
		bound[b.key()] = true
	}
	if publishAll {
		for _, key := range slices.Sorted(maps.Keys(exposed)) {
			if !bound[key] {
				port, proto, _ := parsePort(key)
				bindings = append(bindings, portBinding{ContainerPort: port, Protocol: proto})
			}
		}
	}
	return exposed, bindings
}

func dockerBindings(bindings []portBinding, exposed map[string]struct{}) map[string][]dockerPortBinding {
	result := make(map[string][]dockerPortBinding)
	for key := range exposed {
		result[key] = nil
	}
	for _, b := range bindings {
		port := strconv.Itoa(int(b.HostPort))
		if b.HostPort == 0 {
			port = ""
		}
		key := b.key()
		result[key] = append(result[key], dockerPortBinding{b.HostIP, port})
	}
	return result
}

func (c *container) listedPorts() []map[string]any {
	rows := make([]map[string]any, 0)
	if c.state != "running" || c.attempt == nil || c.attempt.ports == nil {
		return rows
	}
	for _, b := range c.attempt.ports.bound {
		rows = append(rows, map[string]any{
			"IP": b.HostIP, "Type": b.Protocol, "PrivatePort": b.ContainerPort, "PublicPort": b.HostPort,
		})
	}
	return rows
}
