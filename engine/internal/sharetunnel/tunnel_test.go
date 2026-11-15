package sharetunnel

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/OpenFabrica/Fabo/engine/internal/shareedge"
	"github.com/OpenFabrica/Fabo/engine/internal/shareedge/edgetest"
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

// relay starts a real fabo-edge and returns options signed in to it.
func relay(t *testing.T, cfg shareedge.Config) (*edgetest.Edge, Options) {
	t.Helper()
	ca := edgetest.NewCA(t)
	e := edgetest.Start(t, ca, cfg, "")
	return e, clientFor(t, e)
}

// clientFor points the client at e and issues a token.
func clientFor(t *testing.T, e *edgetest.Edge) Options {
	t.Helper()
	t.Setenv("FABO_SHARE_CA", e.CA.Path)
	t.Setenv("FABO_SHARE_CONNECT", e.Addr)
	srv, err := ParseServer(edgetest.EdgeHost)
	if err != nil {
		t.Fatal(err)
	}
	token, err := e.IssueToken("tester")
	if err != nil {
		t.Fatal(err)
	}
	return Options{Server: srv, Token: token, Device: "device-1", Client: "fabo test"}
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

// browser is an HTTPS client that reaches tunnel URLs through the relay.
func browser(e *edgetest.Edge) *http.Client {
	return &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: e.CA.Pool},
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, e.Addr)
		},
		DisableKeepAlives: true,
	}}
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

func TestEchoAndRequestEvent(t *testing.T) {
	e, o := relay(t, shareedge.Config{})
	local := echoServer(t)
	_, rec, tunnelURL := open(t, o, local.Listener.Addr().String())
	if !strings.HasPrefix(tunnelURL, "https://web-") || !strings.HasSuffix(tunnelURL, "."+edgetest.Domain) {
		t.Fatalf("unexpected URL %s", tunnelURL)
	}
	resp, err := browser(e).Get(tunnelURL + "/x")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "hello /x" {
		t.Fatalf("body %q", body)
	}
	req := rec.wait(t, EventRequest, 5*time.Second).Request
	if req.Method != "GET" || req.Path != "/x" || req.Status != 200 || req.Out == 0 {
		t.Fatalf("request event %+v", req)
	}
}

func TestLargeResponse(t *testing.T) {
	e, o := relay(t, shareedge.Config{})
	payload := randomBytes(3 << 20)
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(payload) }))
	defer local.Close()
	_, _, tunnelURL := open(t, o, local.Listener.Addr().String())
	resp, err := browser(e).Get(tunnelURL)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("got %d of %d bytes: %v", len(got), len(payload), err)
	}
}

func TestSmallWindowsCarryLargeBodies(t *testing.T) {
	for _, window := range []uint32{4096, 100} {
		t.Run(fmt.Sprint(window), func(t *testing.T) {
			e, o := relay(t, shareedge.Config{Window: window})
			local := echoServer(t)
			_, _, tunnelURL := open(t, o, local.Listener.Addr().String())
			size := 2 << 20
			if window < 1024 {
				size = 512 << 10
			}
			body := randomBytes(size)
			resp, err := browser(e).Post(tunnelURL, "application/octet-stream", bytes.NewReader(body))
			if err != nil {
				t.Fatal(err)
			}
			got, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			if err != nil || !bytes.Equal(got, body) {
				t.Fatalf("echoed %d of %d bytes: %v", len(got), len(body), err)
			}
		})
	}
}

func TestConcurrentStreamsBothWays(t *testing.T) {
	e, o := relay(t, shareedge.Config{})
	local := echoServer(t)
	_, _, tunnelURL := open(t, o, local.Listener.Addr().String())
	client := browser(e)
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			body := randomBytes(1<<20 + i)
			resp, err := client.Post(tunnelURL, "application/octet-stream", bytes.NewReader(body))
			if err != nil {
				errs <- err
				return
			}
			got, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			if err != nil || !bytes.Equal(got, body) {
				errs <- fmt.Errorf("stream %d echoed %d of %d bytes: %v", i, len(got), len(body), err)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
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

func TestSlowStreamDoesNotBlockOthers(t *testing.T) {
	e, o := relay(t, shareedge.Config{PingInterval: 100 * time.Millisecond})
	o.idle = 600 * time.Millisecond
	stuck := make(chan struct{})
	t.Cleanup(func() { close(stuck) })
	addr := rawServer(t, func(c net.Conn) {
		defer c.Close()
		line, _ := bufio.NewReader(c).ReadString('\n')
		if strings.HasPrefix(line, "SLOW") {
			<-stuck // never read the rest
			return
		}
		io.WriteString(c, "HTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: close\r\n\r\nok")
	})
	_, rec, tunnelURL := open(t, o, addr)
	slow, err := e.Visitor(hostOf(t, tunnelURL))
	if err != nil {
		t.Fatal(err)
	}
	defer slow.Close()
	go func() {
		io.WriteString(slow, "SLOW\n")
		_, _ = slow.Write(randomBytes(4 << 20))
	}()
	time.Sleep(300 * time.Millisecond)
	start := time.Now()
	resp, err := browser(e).Get(tunnelURL)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "ok" || time.Since(start) > 3*time.Second {
		t.Fatalf("fast stream got %q after %v", body, time.Since(start))
	}
	// Pings keep being answered while one stream is stuck: no reconnect.
	rec.none(t, EventReconnecting, 2*time.Second)
}

func TestHalfCloseKeepsReturnedData(t *testing.T) {
	e, o := relay(t, shareedge.Config{})
	reply := randomBytes(2 << 20)
	addr := rawServer(t, func(c net.Conn) {
		defer c.Close()
		_, _ = io.Copy(io.Discard, c) // until the visitor half-closes
		_, _ = c.Write(reply)
	})
	_, _, tunnelURL := open(t, o, addr)
	v, err := e.Visitor(hostOf(t, tunnelURL))
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	io.WriteString(v, "POST / HTTP/1.1\r\nHost: x\r\n\r\nrequest body")
	if err := v.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	_ = v.SetReadDeadline(time.Now().Add(20 * time.Second))
	got, err := io.ReadAll(v)
	if err != nil || !bytes.Equal(got, reply) {
		t.Fatalf("read %d of %d bytes after half-close: %v", len(got), len(reply), err)
	}
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

func TestStopMidTransferLeavesNoGoroutines(t *testing.T) {
	e, o := relay(t, shareedge.Config{})
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		chunk := randomBytes(64 << 10)
		for range 2000 {
			if _, err := w.Write(chunk); err != nil {
				return
			}
		}
	}))
	defer local.Close()
	h, _, tunnelURL := open(t, o, local.Listener.Addr().String())
	resp, err := browser(e).Get(tunnelURL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.CopyN(io.Discard, resp.Body, 1<<20); err != nil {
		t.Fatal(err)
	}
	h.Stop()
	resp.Body.Close()
	deadline := time.Now().Add(5 * time.Second)
	for tunnelGoroutines() > 0 {
		if time.Now().After(deadline) {
			buf := make([]byte, 1<<20)
			t.Fatalf("%d tunnel goroutines left:\n%s", tunnelGoroutines(), buf[:runtime.Stack(buf, true)])
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestRelayRestartKeepsURL(t *testing.T) {
	ca := edgetest.NewCA(t)
	state := t.TempDir()
	e1 := edgetest.Start(t, ca, shareedge.Config{StateDir: state}, "")
	o := clientFor(t, e1)
	local := echoServer(t)
	_, rec, first := open(t, o, local.Listener.Addr().String())
	e1.Stop()
	if r := rec.wait(t, EventReconnecting, 5*time.Second); r.Reason != "edge restarting" {
		t.Fatalf("reconnect reason %q", r.Reason)
	}
	e2 := edgetest.Start(t, ca, shareedge.Config{StateDir: state}, e1.Addr)
	again := rec.wait(t, EventLive, 15*time.Second)
	if again.URL != first {
		t.Fatalf("URL changed across restart: %s then %s", first, again.URL)
	}
	resp, err := browser(e2).Get(again.URL + "/back")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "hello /back" {
		t.Fatalf("body %q", body)
	}
}

func TestStoppedFromAnotherComputerEnds(t *testing.T) {
	_, o := relay(t, shareedge.Config{})
	local := echoServer(t)
	_, rec, tunnelURL := open(t, o, local.Listener.Addr().String())
	api := NewAPI(o.Server, o.Token, "fabo/test")
	if err := api.CloseTunnel(context.Background(), issuedName(tunnelURL), "other-mac"); err != nil {
		t.Fatal(err)
	}
	ended := rec.wait(t, EventEnded, 5*time.Second)
	if !ended.Error || ended.Reason != "stopped from other-mac" {
		t.Fatalf("ended %+v", ended)
	}
}

func TestRefusedTokenEnds(t *testing.T) {
	_, o := relay(t, shareedge.Config{})
	o.Token = "ft_not_a_real_token"
	rec := newRecorder()
	o.Target = Target{Addr: "127.0.0.1:1", Display: "127.0.0.1:1"}
	o.Dial = DialAddr(o.Target.Addr)
	o.OnEvent = rec.on
	h := Start(o)
	defer h.Stop()
	ended := rec.wait(t, EventEnded, 5*time.Second)
	if !ended.Error || !strings.Contains(ended.Reason, "no longer valid") {
		t.Fatalf("ended %+v", ended)
	}
}

func TestUnreachableLocalServiceNotices(t *testing.T) {
	e, o := relay(t, shareedge.Config{})
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	closed := ln.Addr().String()
	ln.Close()
	_, rec, tunnelURL := open(t, o, closed)
	if _, err := browser(e).Get(tunnelURL + "/gone"); err == nil {
		t.Fatal("a request to a dead local service should fail")
	}
	// The unanswered request is reported first, then the notice.
	if req := rec.wait(t, EventRequest, 5*time.Second).Request; req.Path != "/gone" || req.Status != 0 {
		t.Fatalf("request %+v", req)
	}
	if n := rec.wait(t, EventNotice, 5*time.Second); !strings.Contains(n.Text, "nothing is answering on "+closed) {
		t.Fatalf("notice %q", n.Text)
	}
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
