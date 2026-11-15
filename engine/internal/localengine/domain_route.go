//go:build linux || darwin

package localengine

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/OpenFabrica/Fabo/engine/internal/guestwire"
)

var domainSuffixes = []string{"fabo.local", "fabo.localhost"}

// domainLabel holds the extra names a container answers to, comma-separated.
const domainLabel = "dev.fabo.domain"

func domainHost(host string) (string, string) {
	if name, _, err := net.SplitHostPort(host); err == nil {
		host = name
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if len(host) > 253 {
		return "", ""
	}
	for label := range strings.SplitSeq(host, ".") {
		if label == "" || len(label) > 63 {
			return "", ""
		}
		for _, ch := range label {
			if !((ch >= 'a' && ch <= 'z') || (ch >= '0' && ch <= '9') || ch == '-' || ch == '_') {
				return "", ""
			}
		}
	}
	for _, suffix := range domainSuffixes {
		if name, ok := strings.CutSuffix(host, "."+suffix); ok && name != "" {
			return host, name
		}
	}
	return "", ""
}

func matchesDomain(c *container, name string) bool {
	if strings.EqualFold(c.name, name) || len(name) >= 12 && strings.HasPrefix(c.id, name) {
		return true
	}
	service, project := c.labels["com.docker.compose.service"], c.labels["com.docker.compose.project"]
	if service != "" && project != "" && strings.EqualFold(service+"."+project, name) {
		return true
	}
	for label := range strings.SplitSeq(c.labels[domainLabel], ",") {
		label = strings.TrimSpace(label)
		if _, base := domainHost(label); base != "" {
			label = base
		}
		if strings.EqualFold(label, name) {
			return true
		}
	}
	return false
}

type domainTarget struct {
	id                 string
	run                *runAttempt
	ip                 netip.Addr
	label              uint16
	hostNetwork        bool
	published, exposed []uint16
}

func (e *Engine) domainTarget(name string) (domainTarget, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	var chosen *container
	for _, c := range e.containers {
		if c.state == "running" && c.attempt != nil && !c.paused && (c.run.Network.Bridge != "" || c.networkMode() == "host") && matchesDomain(c, name) &&
			(chosen == nil || c.id < chosen.id) {
			chosen = c
		}
	}
	if e.closed || chosen == nil {
		return domainTarget{}, false
	}
	c := chosen
	target := domainTarget{id: c.id, run: c.attempt, ip: c.networkAddress(), hostNetwork: c.networkMode() == "host"}
	if !target.ip.IsValid() {
		return domainTarget{}, false
	}
	if p, err := strconv.ParseUint(c.labels["dev.fabo.http.port"], 10, 16); err == nil {
		target.label = uint16(p)
	}
	for _, binding := range c.portBindings {
		if binding.Protocol == "tcp" {
			target.published = append(target.published, binding.ContainerPort)
		}
	}
	for key := range c.exposedPorts {
		if port, protocol, err := parsePort(key); err == nil && protocol == "tcp" {
			target.exposed = append(target.exposed, port)
		}
	}
	return target, true
}

var commonWebPorts = []uint16{80, 8080, 3000, 8000, 5000, 5173, 4200, 8888, 9000, 8081, 4000, 3001, 15672, 8443}
var nonWebPorts = []uint16{5432, 3306, 6379, 27017, 5672, 4369, 25672, 9042, 11211, 1433, 1521, 2181}

func webPortOrder(input []uint16) []uint16 {
	ports := slices.Clone(input)
	rank := func(port uint16) int {
		if i := slices.Index(commonWebPorts, port); i >= 0 {
			return i
		}
		if slices.Contains(nonWebPorts, port) {
			return len(commonWebPorts) + 1
		}
		return len(commonWebPorts)
	}
	slices.SortFunc(ports, func(a, b uint16) int {
		if diff := rank(a) - rank(b); diff != 0 {
			return diff
		}
		return int(a) - int(b)
	})
	return slices.Compact(ports)
}

func (e *Engine) domainPort(ctx context.Context, target domainTarget) uint16 {
	if target.hostNetwork {
		query, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		rows, err := e.session.Listening(query, target.id)
		if err != nil {
			return 0
		}
		listeners, _ := listeningPorts(rows, true, true)
		if target.label != 0 {
			if slices.Contains(listeners, target.label) {
				return target.label
			}
			return 0
		}
		preferred := append(webPortOrder(target.published), webPortOrder(target.exposed)...)
		for _, port := range append(preferred, webPortOrder(listeners)...) {
			if slices.Contains(listeners, port) {
				return port
			}
		}
		return 0
	}
	if target.label != 0 {
		return target.label
	}
	published, exposed := webPortOrder(target.published), webPortOrder(target.exposed)
	if len(published) == 1 || len(published) == 0 && len(exposed) == 1 {
		if len(published) == 1 {
			return published[0]
		}
		return exposed[0]
	}
	ports := append(published, exposed...)
	query, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if rows, err := e.session.Listening(query, target.id); err == nil {
		listeners, _ := listeningPorts(rows, true, false)
		for _, port := range ports {
			if slices.Contains(listeners, port) {
				return port
			}
		}
		if len(ports) == 0 && len(listeners) != 0 {
			return webPortOrder(listeners)[0]
		}
	}
	if len(ports) != 0 {
		return ports[0]
	}
	return 80
}

// DialDomain resolves the current run and opens a stream to its HTTP service.
func (e *Engine) DialDomain(ctx context.Context, host string) (net.Conn, error) {
	_, name := domainHost(host)
	if e == nil || name == "" {
		return nil, errMissing
	}
	target, found := e.domainTarget(name)
	if !found {
		return nil, errMissing
	}
	port := e.domainPort(ctx, target)
	if port == 0 {
		return nil, errMissing
	}
	select {
	case <-target.run.done:
		return nil, errMissing
	default:
	}
	dst := netip.AddrPortFrom(target.ip, port)
	conn, err := dialPublishedPort(ctx, e.guestRelay, "tcp", guestwire.RelayFramedTCPPort, dst)
	if err != nil {
		return nil, fmt.Errorf("connect to container HTTP service: %w", err)
	}
	if e.guestRelay != nil {
		return guestwire.NewRelayTCPConn(conn), nil
	}
	return conn, nil
}
