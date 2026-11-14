//go:build linux || darwin

package localengine

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/OpenFabrica/FabriBay/engine/internal/guestwire"
)

func TestProxyPersistenceAndBypass(t *testing.T) {
	state := &State{dir: t.TempDir()}
	p, err := OpenProxy(state)
	if err != nil {
		t.Fatal(err)
	}
	request := func(body string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(http.MethodPost, "/fabo/proxy", strings.NewReader(body))
		w := httptest.NewRecorder()
		p.ServeHTTP(w, r)
		return w
	}
	w := request(`{"mode":"custom","http":"user:fixture@127.0.0.1:3128","no_proxy":".corp.invalid,10.0.0.0/8","containers":true}`)
	if w.Code != http.StatusOK {
		t.Fatalf("save status %d", w.Code)
	}
	if w := request(`{"mode":"custom","http":"unsupported://fixture-secret:99999"}`); w.Code != http.StatusBadRequest || strings.Contains(w.Body.String(), "fixture-secret") {
		t.Fatalf("invalid settings response status %d", w.Code)
	}
	info, err := os.Stat(filepath.Join(state.dir, "proxy.json"))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("proxy settings are not private: %v, %v", info, err)
	}
	p, err = OpenProxy(state)
	if err != nil {
		t.Fatal(err)
	}
	env, _ := p.containerEnv(true, false, nil)
	if !slices.Contains(env, "HTTPS_PROXY=http://user:fixture@host.docker.internal:3128") {
		t.Fatal("container did not get the host proxy with HTTPS fallback")
	}
	for _, target := range []string{"https://app.corp.invalid/", "http://10.4.3.2/"} {
		proxy, err := p.proxyForRequest(httptest.NewRequest(http.MethodGet, target, nil))
		if err != nil || proxy != nil {
			t.Fatalf("bypass for %s used the proxy", target)
		}
	}
	r := httptest.NewRequest(http.MethodGet, "https://registry.invalid/", nil)
	proxy, err := p.proxyForRequest(r)
	if err != nil || proxy == nil || proxy.Host != "127.0.0.1:3128" {
		t.Fatal("registry request did not use the saved proxy")
	}
	if w := request(`{"mode":"none","containers":true}`); w.Code != http.StatusOK {
		t.Fatal("could not disable the proxy")
	}
	proxy, err = p.proxyForRequest(r)
	env, _ = p.containerEnv(true, false, nil)
	if err != nil || proxy != nil || len(env) != 0 {
		t.Fatal("disabled proxy was still active")
	}
}

func TestSystemProxyReadsMacNetworkSettings(t *testing.T) {
	p := parseSystemProxy(`<dictionary> {
  ExceptionsList : <array> {
    0 : *.local
  }
  HTTPEnable : 1
  HTTPProxy : proxy.invalid
  HTTPPort : 3128
  ProxyAutoConfigEnable : 1
}`)
	if p.HTTP != "http://proxy.invalid:3128" || p.HTTPS != "" || !p.PAC || !slices.Equal(p.NoProxy, []string{"*.local"}) {
		t.Fatalf("unexpected system proxy report: %+v", p)
	}
}

func TestWindowsProxySnapshotAndContainerOverrides(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://fallback.invalid:8080")
	t.Setenv("HTTPS_PROXY", "")
	p, err := OpenProxy(&State{dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	send := func(handler http.HandlerFunc, method, path, body string, status int) string {
		t.Helper()
		w := httptest.NewRecorder()
		handler(w, httptest.NewRequest(method, path, strings.NewReader(body)))
		if w.Code != status {
			t.Fatalf("proxy settings request returned %d, want %d", w.Code, status)
		}
		return w.Body.String()
	}
	post := func(body string) {
		t.Helper()
		send(p.ServeHTTP, http.MethodPost, "/fabo/proxy", body, http.StatusOK)
	}
	update := func(body string, status int) string {
		t.Helper()
		return send(p.UpdateWindows, http.MethodPut, "/fabo/proxy/system", body, status)
	}
	proxyFor := func(target, host string) {
		t.Helper()
		got, err := p.proxyForRequest(httptest.NewRequest(http.MethodGet, target, nil))
		if err != nil || (got == nil && host != "") || (got != nil && got.Host != host) {
			t.Fatalf("wrong proxy destination for %s", target)
		}
	}
	post(`{"mode":"system","containers":true}`)
	report := update(`{"http":"user:fixture@localhost:7892","https":null,"pac":true,"no_proxy":[".corp.invalid"],"relay":{"7892":41234}}`, http.StatusOK)
	if strings.Contains(report, "fixture") || !strings.Contains(report, `"pac":true`) {
		t.Fatal("Windows proxy report lost PAC state or exposed credentials")
	}
	proxyFor("https://registry.invalid/", "127.0.0.1:41234")
	proxyFor("https://app.corp.invalid/", "")
	env, options := p.containerEnv(false, false, []string{"HTTP_PROXY=http://explicit.invalid:8080"})
	if !slices.Contains(env, "HTTP_PROXY=http://explicit.invalid:8080") ||
		!slices.Contains(env, "HTTPS_PROXY=http://user:fixture@127.0.0.2:7892") ||
		!slices.Equal(options, []string{"fabo.windows-proxy:7892"}) {
		t.Fatal("container proxy settings did not preserve overrides and deduplicate relay ports")
	}
	joined := p.joinedContainerEnv(false, []string{"http_proxy=http://explicit.invalid:8080"}, containerProxyNetwork{relayOptions: options})
	if !slices.Contains(joined, "http_proxy=http://explicit.invalid:8080") ||
		!slices.Contains(joined, "HTTPS_PROXY=http://user:fixture@127.0.0.2:7892") {
		t.Fatal("joiner did not reuse the target's Windows proxy or preserve an override")
	}
	joined = p.joinedContainerEnv(false, nil, containerProxyNetwork{host: true})
	if !slices.Contains(joined, "HTTP_PROXY=http://user:fixture@127.0.0.1:41234") {
		t.Fatal("host-network joiner did not get the mapped WSL port")
	}
	joined = p.joinedContainerEnv(false, nil, containerProxyNetwork{relayOptions: []string{guestwire.WindowsProxyOption + "7893"}})
	if slices.ContainsFunc(joined, func(entry string) bool { return strings.HasPrefix(entry, "HTTP_PROXY=") }) {
		t.Fatal("joiner received a Windows proxy port absent from its network")
	}
	update(`{"http":"localhost:7892","relay":{"7892":0}}`, http.StatusBadRequest)
	proxyFor("https://registry.invalid/", "127.0.0.1:41234")
	post(`{"mode":"custom","http":"127.0.0.1:7892","containers":true}`)
	proxyFor("https://registry.invalid/", "127.0.0.1:41234")
	post(`{"mode":"none","containers":true}`)
	proxyFor("https://registry.invalid/", "")
	post(`{"mode":"system","containers":true}`)
	update(`{"http":"localhost:7892","relay":{}}`, http.StatusOK)
	proxyFor("https://registry.invalid/", "localhost:7892")
	joined = p.joinedContainerEnv(false, nil, containerProxyNetwork{host: true})
	if slices.ContainsFunc(joined, func(entry string) bool { return strings.HasPrefix(entry, "HTTP_PROXY=") }) {
		t.Fatal("joiner fell back to the Windows loopback address after the relay was cleared")
	}
	post(`{"mode":"custom","http":"127.0.0.1:3128","containers":true}`)
	joined = p.joinedContainerEnv(false, nil, containerProxyNetwork{host: true})
	if !slices.Contains(joined, "HTTP_PROXY=http://127.0.0.1:3128") {
		t.Fatal("unmapped custom loopback proxy was discarded")
	}
	post(`{"mode":"system","containers":true}`)
	update(`{"http":null,"https":null,"relay":{}}`, http.StatusOK)
	proxyFor("http://registry.invalid/", "")
}
