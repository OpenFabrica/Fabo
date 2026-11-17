package hostrelay

import (
	"net/netip"
	"slices"
	"strconv"
	"strings"
)

// Loopback is the port of a proxy address whose host is this machine's
// loopback (localhost, 127.0.0.0/8, ::1): "http://user:pw@127.0.0.1:7892/"
// and "127.0.0.1:7892" give 7892. Without an explicit port there is
// nothing to relay. As upstream's hostrelay::loopback, but only the port:
// the engine rewrites the address itself.
func Loopback(raw string) (uint16, bool) {
	rest := raw
	if i := strings.Index(rest, "://"); i >= 0 {
		rest = rest[i+3:]
	}
	if end := strings.IndexAny(rest, "/?#"); end >= 0 {
		rest = rest[:end]
	}
	// A password may hold '@': the host starts after the last one.
	if at := strings.LastIndexByte(rest, '@'); at >= 0 {
		rest = rest[at+1:]
	}
	i := strings.LastIndexByte(rest, ':')
	if i < 0 {
		return 0, false
	}
	host, portText := rest[:i], rest[i+1:]
	port, err := strconv.ParseUint(portText, 10, 16)
	if err != nil || port == 0 {
		return 0, false
	}
	host = strings.ToLower(strings.TrimSuffix(strings.TrimPrefix(host, "["), "]"))
	if host == "localhost" {
		return uint16(port), true
	}
	ip, err := netip.ParseAddr(host)
	if err != nil || !(ip.Is4() && ip.IsLoopback() || ip == netip.IPv6Loopback()) {
		return 0, false
	}
	return uint16(port), true
}

// Targets are the loopback ports among proxy addresses, sorted, once each.
func Targets(addresses ...string) []uint16 {
	var ports []uint16
	for _, a := range addresses {
		if p, ok := Loopback(a); ok {
			ports = append(ports, p)
		}
	}
	slices.Sort(ports)
	return slices.Compact(ports)
}
