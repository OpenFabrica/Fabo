package sharetunnel

import (
	"crypto/rand"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"runtime"
	"strings"
	"testing"
	"time"
)

// recorder collects tunnel events.
type recorder struct {
	ch chan Event
}

func newRecorder() *recorder { return &recorder{ch: make(chan Event, 4096)} }

func (r *recorder) on(e Event) {
	select {
	case r.ch <- e:
	default:
	}
}

// wait returns the next event of type typ, failing after timeout.
func (r *recorder) wait(t *testing.T, typ string, timeout time.Duration) Event {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case e := <-r.ch:
			if e.Type == typ {
				return e
			}
		case <-deadline:
			t.Fatalf("no %s event within %v", typ, timeout)
		}
	}
}

// none fails if an event of type typ arrives within d.
func (r *recorder) none(t *testing.T, typ string, d time.Duration) {
	t.Helper()
	deadline := time.After(d)
	for {
		select {
		case e := <-r.ch:
			if e.Type == typ {
				t.Fatalf("unexpected %s event: %+v", typ, e)
			}
		case <-deadline:
			return
		}
	}
}

// open starts a tunnel to addr and waits for it to go live.
func open(t *testing.T, o Options, addr string) (*Handle, *recorder, string) {
	t.Helper()
	rec := newRecorder()
	o.Target = Target{Addr: addr, Label: "web", Display: addr}
	o.Dial = DialAddr(addr)
	o.OnEvent = rec.on
	h := Start(o)
	t.Cleanup(h.Stop)
	live := rec.wait(t, EventLive, 10*time.Second)
	return h, rec, live.URL
}

func hostOf(t *testing.T, raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u.Hostname()
}

func randomBytes(n int) []byte {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return b
}

// echoServer answers GET with the path and POST with the body it read.
func echoServer(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			fmt.Fprintf(w, "hello %s", r.URL.Path)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// rawServer runs handle for every connection on a loopback port.
func rawServer(t *testing.T, handle func(net.Conn)) string {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go handle(c)
		}
	}()
	return ln.Addr().String()
}

// tunnelGoroutines counts goroutines running tunnel or stream code.
func tunnelGoroutines() int {
	buf := make([]byte, 1<<20)
	buf = buf[:runtime.Stack(buf, true)]
	n := 0
	for _, g := range strings.Split(string(buf), "\n\n") {
		if strings.Contains(g, "sharetunnel.(*") || strings.Contains(g, "sharetunnel.Start") || strings.Contains(g, "sharewire.(*Stream)") {
			n++
		}
	}
	return n
}

func TestParseAddr(t *testing.T) {
	cases := map[string]Target{
		"3000":             {Addr: "localhost:3000", Display: "localhost:3000"},
		"localhost:8080":   {Addr: "localhost:8080", Display: "localhost:8080"},
		"api.internal:80":  {Addr: "api.internal:80", Label: "api", Display: "api.internal:80"},
		"[::1]:9000":       {Addr: "[::1]:9000", Display: "[::1]:9000"},
		"192.168.1.5:3000": {Addr: "192.168.1.5:3000", Display: "192.168.1.5:3000"},
	}
	for in, want := range cases {
		if got, ok := ParseAddr(in); !ok || got != want {
			t.Errorf("%s: %+v %v", in, got, ok)
		}
	}
	for _, in := range []string{"", "0", "65536", "123456", "web", "ps", ":80", "host:", "host:0", "run"} {
		if IsShorthand(in) {
			t.Errorf("%q should not be a shorthand", in)
		}
	}
}

func TestSniffing(t *testing.T) {
	if m, p, ok := parseRequestLine([]byte("GET /a?b HTTP/1.1\r\nHost: x")); !ok || m != "GET" || p != "/a?b" {
		t.Fatal(m, p, ok)
	}
	for _, bad := range []string{"get / HTTP/1.1\r\n", "GET /\r\n", "GET / FTP\r\n", "GET / HTTP/1.1"} {
		if _, _, ok := parseRequestLine([]byte(bad)); ok {
			t.Errorf("%q parsed", bad)
		}
	}
	if code, ok := parseStatusLine([]byte("HTTP/1.1 404 Not Found\r\n")); !ok || code != 404 {
		t.Fatal(code, ok)
	}
	if _, ok := parseStatusLine([]byte("HTTP/1.1 2000\r\n")); ok {
		t.Fatal("bad status parsed")
	}
}

func TestConfigRoundTripAndServerParsing(t *testing.T) {
	path := t.TempDir() + "/share.json"
	cfg, err := LoadConfig(path)
	if err != nil || len(cfg.Device) != 32 {
		t.Fatalf("new config %+v %v", cfg, err)
	}
	cfg.Token, cfg.Login, cfg.Server = "ft_x", "me", "edge.example.com"
	if err := SaveConfig(path, cfg); err != nil {
		t.Fatal(err)
	}
	again, err := LoadConfig(path)
	if err != nil || again != cfg {
		t.Fatalf("reloaded %+v %v", again, err)
	}
	for in, want := range map[string]string{"https://Edge.Example.com/": "edge.example.com", "edge.example.com:8443": "edge.example.com:8443"} {
		srv, err := ParseServer(in)
		if err != nil || srv.String() != want {
			t.Errorf("%s: %v %v", in, srv, err)
		}
	}
	if _, err := ParseServer(""); err != ErrNoServer {
		t.Fatalf("empty server: %v", err)
	}
}

// A token is only sent to the relay that issued it: pointing the client at
// another relay, by config or FABO_SHARE_SERVER, reads as signed out.
func TestTokenStaysWithItsRelay(t *testing.T) {
	t.Setenv("FABO_SHARE_TOKEN", "")
	t.Setenv("FABO_SHARE_SERVER", "")
	cfg := Config{Server: "a.example.com"}
	cfg.SetToken("ft_a", "me")
	if cfg.EffectiveToken() != "ft_a" || cfg.TokenServer != "a.example.com" {
		t.Fatalf("issued token %+v", cfg)
	}
	t.Setenv("FABO_SHARE_SERVER", "b.example.com")
	if got := cfg.EffectiveToken(); got != "" {
		t.Fatalf("A's token would go to B: %q", got)
	}
	// Saved for B while A was pinned: removing the pin must not pair them.
	t.Setenv("FABO_SHARE_SERVER", "")
	cfg.Server = "b.example.com"
	if got := cfg.EffectiveToken(); got != "" {
		t.Fatalf("A's token would go to B after the pin left: %q", got)
	}
	// A token saved before TokenServer existed belongs to no relay.
	if (Config{Server: "a.example.com", Token: "ft_old"}).EffectiveToken() != "" {
		t.Fatal("an unbound token was used")
	}
}
