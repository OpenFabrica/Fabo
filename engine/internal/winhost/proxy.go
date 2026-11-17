package winhost

import (
	"encoding/json"
	"net"
	"net/url"
	"strconv"
	"strings"

	"github.com/OpenFabrica/Fabo/engine/internal/hostrelay"
)

// SystemProxy is Windows' proxy as the engine's PUT /fabo/proxy/system
// takes it (engine/internal/localengine/proxy_wsl.go).
type SystemProxy struct {
	HTTP    *string  `json:"http"`
	HTTPS   *string  `json:"https"`
	NoProxy []string `json:"no_proxy"`
	PAC     bool     `json:"pac"`
}

// ParseSystemProxy reads the values under HKCU\...\Internet Settings:
// ProxyEnable, ProxyServer ("host:port", or "http=h:p;https=h:p;…"),
// ProxyOverride ("a;b;<local>") and whether AutoConfigURL is set.
func ParseSystemProxy(enabled bool, server, override string, pac bool) SystemProxy {
	p := SystemProxy{NoProxy: []string{}, PAC: pac}
	if !enabled {
		return p
	}
	server = strings.TrimSpace(server)
	if strings.Contains(server, "=") {
		for _, part := range strings.Split(server, ";") {
			scheme, value, ok := strings.Cut(strings.TrimSpace(part), "=")
			if !ok || value == "" {
				continue
			}
			v := value
			switch strings.ToLower(scheme) {
			case "http":
				p.HTTP = &v
			case "https":
				p.HTTPS = &v
			}
		}
	} else if server != "" {
		h, s := server, server
		p.HTTP, p.HTTPS = &h, &s
	}
	for _, host := range strings.Split(override, ";") {
		if host = strings.TrimSpace(host); host != "" && host != "<local>" {
			p.NoProxy = append(p.NoProxy, host)
		}
	}
	return p
}

// Relayed keeps a proxy on Windows' own loopback (127.0.0.1, localhost),
// which most Windows proxy apps use, only when the proxy relay leads to its
// port (relay maps a Windows port to the distro's, from hostrelay.Client).
// Without it the engine would send every pull to a port nothing listens on
// in WSL, so such a proxy is dropped and pulls connect directly; upstream
// pushed it as set.
func (p SystemProxy) Relayed(relay map[uint16]uint16) SystemProxy {
	keep := func(v *string) *string {
		if v == nil || !onLoopback(*v) {
			return v
		}
		if port, ok := hostrelay.Loopback(*v); ok && relay[port] != 0 {
			return v
		}
		return nil
	}
	p.HTTP, p.HTTPS = keep(p.HTTP), keep(p.HTTPS)
	return p
}

// onLoopback reports whether a proxy address names this machine.
func onLoopback(v string) bool {
	raw := v
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		_, ok := hostrelay.Loopback(v)
		return ok
	}
	host := u.Hostname()
	ip := net.ParseIP(host)
	return strings.EqualFold(host, "localhost") || (ip != nil && ip.IsLoopback())
}

// Addresses are the proxy's addresses that are set.
func (p SystemProxy) Addresses() []string {
	var out []string
	for _, v := range []*string{p.HTTP, p.HTTPS} {
		if v != nil {
			out = append(out, *v)
		}
	}
	return out
}

// ProxyUpdate is the body of the engine's PUT /fabo/proxy/system: p with
// the loopback proxies the relay does not reach left out, and the relay
// map. The map is always present, so a relay that has gone is withdrawn
// (the engine keeps its old map when the field is missing), and holds no
// zero port, which the engine refuses.
func ProxyUpdate(p SystemProxy, relay map[uint16]uint16) []byte {
	ports := map[string]uint16{}
	for target, local := range relay {
		if target != 0 && local != 0 {
			ports[strconv.Itoa(int(target))] = local
		}
	}
	body, _ := json.Marshal(struct {
		SystemProxy
		Relay map[string]uint16 `json:"relay"`
	}{p.Relayed(relay), ports})
	return body
}
