//go:build linux || darwin

package localengine

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/OpenFabrica/Fabo/engine/internal/shareedge"
	"github.com/OpenFabrica/Fabo/engine/internal/shareedge/edgetest"
)

// shareHarness is a PublicShares served over HTTP, signed in to a real relay.
type shareHarness struct {
	t      *testing.T
	edge   *edgetest.Edge
	shares *PublicShares
	http   *httptest.Server
	config string
}

func newShareHarness(t *testing.T, cfg shareedge.Config, opts PublicShareOptions) *shareHarness {
	t.Helper()
	ca := edgetest.NewCA(t)
	e := edgetest.Start(t, ca, cfg, "")
	t.Setenv("FABO_SHARE_CA", ca.Path)
	t.Setenv("FABO_SHARE_CONNECT", e.Addr)
	t.Setenv("FABO_SHARE_SERVER", "")
	t.Setenv("FABO_SHARE_TOKEN", "")
	opts.ConfigPath = t.TempDir() + "/share.json"
	opts.Version = "0.0.0-test"
	p, err := OpenPublicShares(opts)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(p)
	t.Cleanup(func() {
		srv.Close()
		p.Close()
	})
	h := &shareHarness{t: t, edge: e, shares: p, http: srv, config: opts.ConfigPath}
	h.call("POST", "/fabo/share/login", fmt.Sprintf(`{"server":%q}`, edgetest.EdgeHost), nil)
	return h
}

// call sends one JSON request and decodes the reply; it returns the status.
func (h *shareHarness) call(method, path, body string, out any) int {
	h.t.Helper()
	req, _ := http.NewRequest(method, h.http.URL+path, strings.NewReader(body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	if out != nil {
		_ = json.NewDecoder(resp.Body).Decode(out)
	}
	return resp.StatusCode
}

// signIn logs in with an operator token for name.
func (h *shareHarness) signIn(name string) string {
	h.t.Helper()
	token, err := h.edge.IssueToken(name)
	if err != nil {
		h.t.Fatal(err)
	}
	if code := h.call("POST", "/fabo/share/login", fmt.Sprintf(`{"token":%q}`, token), nil); code != 200 {
		h.t.Fatalf("token login: %d", code)
	}
	return token
}

type shareReport struct {
	Account struct {
		SignedIn bool   `json:"signedIn"`
		Login    string `json:"login"`
		Server   string `json:"server"`
		Pending  *struct {
			Code string `json:"code"`
		} `json:"pending"`
	} `json:"account"`
	Shares []struct {
		ID, Target, State, URL, Owner, Reason string
		Requests                              uint64
	} `json:"shares"`
}

func (h *shareHarness) report() shareReport {
	h.t.Helper()
	var r shareReport
	h.call("GET", "/fabo/share", "", &r)
	return r
}

// waitFor polls the report until ok, failing after a few seconds.
func (h *shareHarness) waitFor(what string, ok func(shareReport) bool) shareReport {
	h.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		r := h.report()
		if ok(r) {
			return r
		}
		if time.Now().After(deadline) {
			h.t.Fatalf("timed out waiting for %s: %+v", what, r)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func localHTTP(t *testing.T) string {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok") }))
	t.Cleanup(srv.Close)
	return srv.Listener.Addr().String()
}

func TestShareRunLeaseEndsWithRequest(t *testing.T) {
	h := newShareHarness(t, shareedge.Config{}, PublicShareOptions{})
	h.signIn("cli")
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "POST", h.http.URL+"/fabo/share/run", strings.NewReader(fmt.Sprintf(`{"target":%q}`, localHTTP(t))))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	lines := bufio.NewScanner(resp.Body)
	var types []string
	for lines.Scan() {
		var e struct{ Type string }
		_ = json.Unmarshal(lines.Bytes(), &e)
		types = append(types, e.Type)
		if e.Type == "live" {
			break
		}
	}
	if len(types) < 3 || types[0] != "created" || types[1] != "connecting" {
		t.Fatalf("event stream began %v", types)
	}
	if r := h.report(); len(r.Shares) != 1 || r.Shares[0].Owner != "cli" || r.Shares[0].State != "live" {
		t.Fatalf("report %+v", r)
	}
	// The CLI going away (Ctrl+C, kill, lost connection) ends the lease.
	cancel()
	resp.Body.Close()
	h.waitFor("the share to go", func(r shareReport) bool { return len(r.Shares) == 0 })
}

func TestShareObserversDoNotOwnTheTunnel(t *testing.T) {
	h := newShareHarness(t, shareedge.Config{}, PublicShareOptions{})
	h.signIn("app")
	var created struct{ ID string }
	if code := h.call("POST", "/fabo/share", fmt.Sprintf(`{"target":%q}`, localHTTP(t)), &created); code != 201 {
		t.Fatalf("create: %d", code)
	}
	h.waitFor("live", func(r shareReport) bool { return len(r.Shares) == 1 && r.Shares[0].State == "live" })
	for range 2 {
		ctx, cancel := context.WithCancel(context.Background())
		req, _ := http.NewRequestWithContext(ctx, "GET", h.http.URL+"/fabo/share/"+created.ID+"/events", nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil || resp.StatusCode != 200 {
			t.Fatalf("watch: %v %v", resp, err)
		}
		cancel()
		resp.Body.Close()
	}
	time.Sleep(200 * time.Millisecond)
	if r := h.report(); len(r.Shares) != 1 || r.Shares[0].State != "live" {
		t.Fatalf("observers stopped the share: %+v", r)
	}
	if code := h.call("DELETE", "/fabo/share/"+created.ID, "", nil); code != 204 {
		t.Fatalf("delete: %d", code)
	}
	if r := h.report(); len(r.Shares) != 0 {
		t.Fatalf("share left after delete: %+v", r)
	}
}

func TestLogoutAndServerSwitchStopTunnels(t *testing.T) {
	h := newShareHarness(t, shareedge.Config{}, PublicShareOptions{})
	for _, leave := range []string{
		`/fabo/share/logout|`,
		`/fabo/share/login|{"server":"elsewhere.example.com"}`,
	} {
		path, body, _ := strings.Cut(leave, "|")
		h.call("POST", "/fabo/share/login", fmt.Sprintf(`{"server":%q}`, edgetest.EdgeHost), nil)
		h.signIn("switcher")
		h.call("POST", "/fabo/share", fmt.Sprintf(`{"target":%q}`, localHTTP(t)), nil)
		h.waitFor("live", func(r shareReport) bool { return len(r.Shares) == 1 && r.Shares[0].State == "live" })
		h.call("POST", path, body, nil)
		r := h.report()
		if len(r.Shares) != 0 || r.Account.SignedIn {
			t.Fatalf("%s left %+v", path, r)
		}
		raw, _ := os.ReadFile(h.config)
		if strings.Contains(string(raw), "ft_") {
			t.Fatalf("%s left the token on disk: %s", path, raw)
		}
	}
	// Re-selecting the relay already in effect keeps the sign-in.
	h.call("POST", "/fabo/share/login", fmt.Sprintf(`{"server":%q}`, edgetest.EdgeHost), nil)
	h.signIn("keeper")
	h.call("POST", "/fabo/share/login", fmt.Sprintf(`{"server":"https://%s/"}`, strings.ToUpper(edgetest.EdgeHost)), nil)
	if r := h.report(); !r.Account.SignedIn || r.Account.Login != "keeper" {
		t.Fatalf("same relay dropped the sign-in: %+v", r)
	}
}

func TestSupersededDeviceFlowIsDropped(t *testing.T) {
	var polls atomic.Int32
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/login/device/code":
			io.WriteString(w, `{"device_code":"dc","user_code":"WXYZ-0000","verification_uri":"https://github.com/login/device","expires_in":60,"interval":1}`)
		case "/login/oauth/access_token":
			polls.Add(1)
			io.WriteString(w, `{"access_token":"gho"}`)
		case "/user":
			io.WriteString(w, `{"id":9,"login":"octo"}`)
		}
	}))
	defer gh.Close()
	h := newShareHarness(t, shareedge.Config{GitHubClientID: "c", GitHubAllow: []string{"*"}, GitHubURL: gh.URL, GitHubAPI: gh.URL}, PublicShareOptions{})
	var view accountView
	if code := h.call("POST", "/fabo/share/login", `{"github":true}`, &view); code != 200 || view.Pending == nil || view.Pending.Code != "WXYZ-0000" {
		t.Fatalf("device flow start: %d %+v", code, view)
	}
	// A token sign-in replaces the device flow before it is approved.
	h.signIn("operator")
	time.Sleep(2500 * time.Millisecond)
	if r := h.report(); r.Account.Login != "operator" || r.Account.Pending != nil {
		t.Fatalf("the superseded GitHub sign-in won: %+v (polls %d)", r.Account, polls.Load())
	}
	// Without interference the device flow completes on its own.
	h.call("POST", "/fabo/share/login", `{"github":true}`, nil)
	h.waitFor("GitHub sign-in", func(r shareReport) bool { return r.Account.Login == "octo" && r.Account.Pending == nil })
}

func TestShareContainerTarget(t *testing.T) {
	local := localHTTP(t)
	var dials atomic.Int32
	opts := PublicShareOptions{
		ResolveContainer: func(_ context.Context, name string) (string, error) {
			if name != "web" {
				return "", fmt.Errorf("%w: no port or container named %s", errMissing, name)
			}
			return "web", nil
		},
		DialContainer: func(ctx context.Context, name string) (net.Conn, error) {
			dials.Add(1)
			var d net.Dialer
			return d.DialContext(ctx, "tcp", local)
		},
	}
	h := newShareHarness(t, shareedge.Config{}, opts)
	h.signIn("containers")
	if code := h.call("POST", "/fabo/share", `{"target":"missing"}`, nil); code != 404 {
		t.Fatalf("missing container: %d", code)
	}
	h.call("POST", "/fabo/share", `{"target":"web"}`, nil)
	r := h.waitFor("live", func(r shareReport) bool { return len(r.Shares) == 1 && r.Shares[0].URL != "" })
	if r.Shares[0].Target != "web" || !strings.HasPrefix(r.Shares[0].URL, "https://web-") {
		t.Fatalf("share %+v", r.Shares[0])
	}
	client := &http.Client{Transport: &http.Transport{
		TLSClientConfig: h.edge.CA.TLSClientConfig(),
		// A request is reported when its connection ends, as upstream does.
		DisableKeepAlives: true,
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, h.edge.Addr)
		},
	}}
	resp, err := client.Get(r.Shares[0].URL)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "ok" || dials.Load() != 1 {
		t.Fatalf("body %q, %d dials", body, dials.Load())
	}
	h.waitFor("the request count", func(r shareReport) bool { return r.Shares[0].Requests == 1 })
}

func TestShareNeedsSignInAndRelay(t *testing.T) {
	h := newShareHarness(t, shareedge.Config{}, PublicShareOptions{})
	var out map[string]string
	if code := h.call("POST", "/fabo/share", `{"target":"3000"}`, &out); code != 400 || !strings.Contains(out["message"], "not signed in") {
		t.Fatalf("signed out: %d %v", code, out)
	}
	h.call("POST", "/fabo/share/login", `{"server":""}`, nil)
	if code := h.call("POST", "/fabo/share", `{"target":"3000"}`, &out); code != 400 || !strings.Contains(out["message"], "no share relay") {
		t.Fatalf("no relay: %d %v", code, out)
	}
}

// The account view is a snapshot: encoding it after the lock is released
// never reads the pending sign-in a poll is still writing.
func TestAccountViewDoesNotShareThePendingLogin(t *testing.T) {
	t.Setenv("FABO_SHARE_SERVER", "")
	p, err := OpenPublicShares(PublicShareOptions{ConfigPath: t.TempDir() + "/share.json"})
	if err != nil {
		t.Fatal(err)
	}
	p.login = &pendingLogin{Code: "ABCD"}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := range 2000 {
			p.mu.Lock()
			p.login.Error = fmt.Sprint("attempt ", i)
			p.mu.Unlock()
		}
	}()
	for range 2000 {
		if _, err := json.Marshal(p.report()); err != nil {
			t.Fatal(err)
		}
	}
	<-done
}
