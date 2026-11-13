//go:build linux || darwin

package localengine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/libracoder/kiri/engine/internal/guestwire"

	"github.com/google/go-containerregistry/pkg/v1/remote"
	"golang.org/x/net/http/httpproxy"
)

type proxyConfig struct {
	Mode       string `json:"mode"`
	HTTP       string `json:"http"`
	HTTPS      string `json:"https"`
	NoProxy    string `json:"no_proxy"`
	Containers bool   `json:"containers"`
}

type effectiveProxy struct {
	HTTP    string   `json:"http"`
	HTTPS   string   `json:"https"`
	NoProxy []string `json:"no_proxy"`
	PAC     bool     `json:"pac"`
}

// Proxy stores host proxy settings independently of the guest lifecycle.
type Proxy struct {
	mu        sync.Mutex
	state     *State
	config    proxyConfig
	cached    effectiveProxy
	windows   *effectiveProxy
	relay     map[uint16]uint16
	expiry    time.Time
	transport *http.Transport
}

func OpenProxy(state *State) (*Proxy, error) {
	p := &Proxy{state: state, config: proxyConfig{Mode: "system"}}
	raw, err := os.ReadFile(filepath.Join(state.dir, "proxy.json"))
	if err == nil {
		if err := json.Unmarshal(raw, &p.config); err != nil {
			return nil, fmt.Errorf("read proxy settings: %w", err)
		}
		if err := p.config.validate(); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	p.transport = remote.DefaultTransport.(*http.Transport).Clone()
	p.transport.Proxy = p.proxyForRequest
	return p, nil
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
	case http.MethodPost:
		var config proxyConfig
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 32<<10)).Decode(&config); err != nil {
			writeError(w, fmt.Errorf("%w: invalid proxy settings", errInvalid))
			return
		}
		if err := config.validate(); err != nil {
			writeError(w, err)
			return
		}
		if err := p.save(config); err != nil {
			writeError(w, err)
			return
		}
	default:
		w.Header().Set("Allow", "GET, POST")
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	p.report(w)
}

func (p *Proxy) report(w http.ResponseWriter) {
	config, effective, _ := p.effective()
	effective.HTTP = redactProxy(effective.HTTP)
	effective.HTTPS = redactProxy(effective.HTTPS)
	writeJSON(w, http.StatusOK, map[string]any{"settings": config, "effective": effective})
}

func (p *Proxy) save(config proxyConfig) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := saveJSON(p.state.dir, "proxy.json", config); err != nil {
		return err
	}
	p.config, p.expiry = config, time.Time{}
	p.transport.CloseIdleConnections()
	return nil
}

func (p *Proxy) effective() (proxyConfig, effectiveProxy, map[uint16]uint16) {
	p.mu.Lock()
	defer p.mu.Unlock()
	switch p.config.Mode {
	case "custom":
		return p.config, effectiveProxy{HTTP: p.config.HTTP, HTTPS: p.config.HTTPS, NoProxy: splitProxyList(p.config.NoProxy)}, p.relay
	case "none":
		return p.config, effectiveProxy{NoProxy: []string{}}, nil
	}
	if p.windows != nil {
		return p.config, *p.windows, p.relay
	}
	if time.Now().After(p.expiry) {
		p.cached = systemProxy()
		p.expiry = time.Now().Add(10 * time.Second)
	}
	return p.config, p.cached, nil
}

func (p *Proxy) proxyForRequest(r *http.Request) (*url.URL, error) {
	_, active, relay := p.effective()
	httpProxy, httpsProxy := active.HTTP, active.HTTPS
	if httpsProxy == "" {
		httpsProxy = httpProxy
	}
	conf := httpproxy.Config{HTTPProxy: httpProxy, HTTPSProxy: httpsProxy, NoProxy: strings.Join(active.NoProxy, ",")}
	u, err := conf.ProxyFunc()(r.URL)
	if u != nil {
		if port := relay[windowsProxyPort(u)]; port != 0 {
			u.Host = net.JoinHostPort("127.0.0.1", strconv.Itoa(int(port)))
		}
	}
	return u, err
}

func (e *Engine) registryTransport() http.RoundTripper {
	if e.proxy == nil {
		return remote.DefaultTransport
	}
	return e.proxy.transport
}

// containerProxyNetwork describes the proxy routes installed for a live
// namespace. A joiner uses those routes without opening another listener.
type containerProxyNetwork struct {
	host         bool
	relayOptions []string
}

func (p *Proxy) containerEnv(vm, hostNetwork bool, env []string) ([]string, []string) {
	return p.applyContainerProxy(vm, env, containerProxyNetwork{host: hostNetwork}, false)
}

func (p *Proxy) joinedContainerEnv(vm bool, env []string, network containerProxyNetwork) []string {
	env, _ = p.applyContainerProxy(vm, slices.Clone(env), network, true)
	return env
}

// applyContainerProxy preserves explicit Docker entries. For a new
// namespace it also returns the Windows ports the guest should bind.
func (p *Proxy) applyContainerProxy(vm bool, env []string, network containerProxyNetwork, joined bool) ([]string, []string) {
	if p == nil {
		return env, nil
	}
	config, active, relay := p.effective()
	if !config.Containers {
		return env, nil
	}
	set := func(key, value string) bool {
		for _, entry := range env {
			if strings.HasPrefix(entry, key+"=") {
				return false
			}
		}
		env = append(env, key+"="+value)
		return true
	}
	https := active.HTTPS
	if https == "" {
		https = active.HTTP
	}
	var options []string
	for _, item := range []struct{ key, value string }{{"HTTP_PROXY", active.HTTP}, {"HTTPS_PROXY", https}} {
		if item.value == "" {
			continue
		}
		u, _ := url.Parse(item.value)
		port := windowsProxyPort(u)
		local := relay[port]
		option := guestwire.WindowsProxyOption + strconv.Itoa(int(port))
		if local != 0 {
			if joined && !network.host && !slices.Contains(network.relayOptions, option) {
				continue
			}
			address := guestwire.WindowsProxyAddress
			if network.host {
				address, port = "127.0.0.1", local
			}
			u.Host = net.JoinHostPort(address, strconv.Itoa(int(port)))
			item.value = u.String()
		} else if joined && config.Mode == "system" && relay != nil && windowsLoopback(u) {
			// This snapshot describes Windows loopback, which is unreachable
			// from the joined namespace until the Windows relay is installed.
			continue
		} else if vm && u != nil && isLoopbackProxy(u.Hostname()) {
			u.Host = net.JoinHostPort("host.docker.internal", proxyPort(u))
			item.value = u.String()
		}
		upper := set(item.key, item.value)
		lower := set(strings.ToLower(item.key), item.value)
		if !joined && !network.host && (upper || lower) && local != 0 && !slices.Contains(options, option) {
			options = append(options, option)
		}
	}
	if active.HTTP != "" || https != "" {
		bypass := append([]string{}, active.NoProxy...)
		bypass = append(bypass, "localhost", "127.0.0.1", ".kiri.local", ".kiri.localhost", "host.docker.internal")
		noProxy := strings.Join(bypass, ",")
		set("NO_PROXY", noProxy)
		set("no_proxy", noProxy)
	}
	return env, options
}

// windowsProxyPort matches the loopback hosts and explicit ports supported
// by kiri-win's relay.
func windowsProxyPort(u *url.URL) uint16 {
	if !windowsLoopback(u) {
		return 0
	}
	port, _ := strconv.ParseUint(u.Port(), 10, 16)
	return uint16(port)
}

func windowsLoopback(u *url.URL) bool {
	if u == nil {
		return false
	}
	host := u.Hostname()
	ip, _ := netip.ParseAddr(host)
	return strings.EqualFold(host, "localhost") || ip.IsLoopback()
}

func proxyPort(u *url.URL) string {
	if port := u.Port(); port != "" {
		return port
	}
	switch u.Scheme {
	case "https":
		return "443"
	case "socks5", "socks5h":
		return "1080"
	default:
		return "80"
	}
}

func isLoopbackProxy(host string) bool {
	return strings.EqualFold(host, "localhost") || host == "127.0.0.1"
}

func (p *proxyConfig) validate() error {
	switch p.Mode {
	case "system", "none", "custom":
	default:
		return fmt.Errorf("%w: mode must be system, custom or none", errInvalid)
	}
	p.NoProxy = strings.TrimSpace(p.NoProxy)
	p.HTTP, p.HTTPS = strings.TrimSpace(p.HTTP), strings.TrimSpace(p.HTTPS)
	if p.Mode == "custom" {
		for _, item := range []struct {
			key   string
			value *string
		}{{"HTTP", &p.HTTP}, {"HTTPS", &p.HTTPS}} {
			normalized, err := normalizeProxy(*item.value)
			if err != nil {
				return fmt.Errorf("%w: invalid %s proxy address", errInvalid, item.key)
			}
			*item.value = normalized
		}
	}
	return nil
}

func normalizeProxy(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || u.RawQuery != "" || u.Fragment != "" || u.Path != "" && u.Path != "/" {
		return "", fmt.Errorf("invalid proxy address")
	}
	if port := u.Port(); port != "" {
		if n, err := strconv.ParseUint(port, 10, 16); err != nil || n == 0 {
			return "", fmt.Errorf("invalid proxy port")
		}
	}
	switch u.Scheme {
	case "http", "https", "socks5", "socks5h":
		return u.String(), nil
	default:
		return "", fmt.Errorf("unsupported proxy scheme")
	}
}

func redactProxy(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	u.User = nil
	return u.String()
}

func splitProxyList(raw string) []string {
	return append([]string{}, strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' || r == '\n' })...)
}

func systemProxy() effectiveProxy {
	if runtime.GOOS == "darwin" {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, "/usr/sbin/scutil", "--proxy").Output()
		if err != nil {
			return effectiveProxy{NoProxy: []string{}}
		}
		return parseSystemProxy(string(out))
	}
	conf := httpproxy.FromEnvironment()
	httpProxy, _ := normalizeProxy(conf.HTTPProxy)
	httpsProxy, _ := normalizeProxy(conf.HTTPSProxy)
	return effectiveProxy{HTTP: httpProxy, HTTPS: httpsProxy, NoProxy: splitProxyList(conf.NoProxy)}
}

func parseSystemProxy(text string) effectiveProxy {
	fields := make(map[string]string)
	out := effectiveProxy{NoProxy: []string{}}
	exceptions := false
	for line := range strings.Lines(text) {
		line = strings.TrimSpace(line)
		if line == "}" && exceptions {
			exceptions = false
		}
		key, value, ok := strings.Cut(line, " : ")
		if !ok {
			continue
		}
		if key == "ExceptionsList" {
			exceptions = true
		} else if exceptions {
			out.NoProxy = append(out.NoProxy, value)
		} else {
			fields[key] = value
		}
	}
	proxy := func(prefix string) string {
		if fields[prefix+"Enable"] != "1" || fields[prefix+"Proxy"] == "" {
			return ""
		}
		port := fields[prefix+"Port"]
		if port == "" {
			port = "8080"
		}
		value, _ := normalizeProxy(net.JoinHostPort(fields[prefix+"Proxy"], port))
		return value
	}
	out.HTTP, out.HTTPS = proxy("HTTP"), proxy("HTTPS")
	out.PAC = fields["ProxyAutoConfigEnable"] == "1"
	return out
}
