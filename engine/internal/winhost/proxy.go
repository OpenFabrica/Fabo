package winhost

import (
	"net"
	"net/url"
	"strings"
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

// WithoutLoopback drops a proxy on Windows' own loopback (127.0.0.1,
// localhost), which most Windows proxy apps use. Containers and image
// pulls in WSL cannot reach it without upstream's proxy relay, which is
// not ported: the engine would send every pull to a port nothing listens
// on in WSL. Without it they connect directly.
func (p SystemProxy) WithoutLoopback() SystemProxy {
	loopback := func(v *string) bool {
		if v == nil {
			return false
		}
		raw := *v
		if !strings.Contains(raw, "://") {
			raw = "http://" + raw
		}
		u, err := url.Parse(raw)
		if err != nil {
			return false
		}
		host := u.Hostname()
		ip := net.ParseIP(host)
		return strings.EqualFold(host, "localhost") || (ip != nil && ip.IsLoopback())
	}
	if loopback(p.HTTP) {
		p.HTTP = nil
	}
	if loopback(p.HTTPS) {
		p.HTTPS = nil
	}
	return p
}
