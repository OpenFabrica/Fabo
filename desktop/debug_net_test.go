package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// freePort finds a loopback port nothing listens on.
func freePort(t *testing.T, network string) int {
	t.Helper()
	if network == "udp" {
		pc, err := net.ListenPacket("udp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer pc.Close()
		return pc.LocalAddr().(*net.UDPAddr).Port
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// waitFor polls a run until cond holds on its snapshot.
func waitFor(t *testing.T, r *debugRun, what string, cond func(debugSnap) bool) debugSnap {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if snap := r.snapshot(); cond(snap) {
			return snap
		}
		time.Sleep(10 * time.Millisecond)
	}
	snap := r.snapshot()
	t.Fatalf("timed out waiting for %s; state %q problem %q lines %d", what, snap.state, snap.problem, len(snap.lines))
	return snap
}

// received reports whether a run got data equal to want.
func received(want string) func(debugSnap) bool {
	return func(s debugSnap) bool {
		for _, l := range s.lines {
			if l.Dir == lineReceived && string(l.Data) == want {
				return true
			}
		}
		return false
	}
}

func TestUnescapeAndHex(t *testing.T) {
	got, err := unescape(`PING\r\n\x41\\\0`)
	if err != nil || got != "PING\r\nA\\\x00" {
		t.Fatalf("unescape = %q, %v", got, err)
	}
	for _, bad := range []string{`a\`, `\q`, `\x4`, `\xzz`} {
		if _, err := unescape(bad); err == nil {
			t.Errorf("unescape(%q) accepted", bad)
		}
	}
	data, err := parseHex("50 49:4e 0x47\n0d0a")
	if err != nil || string(data) != "PING\r\n" {
		t.Fatalf("parseHex = %q, %v", data, err)
	}
	if _, err := parseHex("5"); err == nil {
		t.Error("odd hex accepted")
	}
	pairs, text := hexDump([]byte("A\n"))
	if pairs != "41 0a" || text != "A." {
		t.Fatalf("hexDump = %q %q", pairs, text)
	}
	if got := decodeText([]byte("ok\r\n\xff"), "UTF-8"); got != `ok\r\n\xff` {
		t.Fatalf("decodeText = %q", got)
	}
	gbk, err := encodeText("中文", "GBK", false)
	if err != nil || len(gbk) != 4 || decodeText(gbk, "GBK") != "中文" {
		t.Fatalf("GBK round trip = %x, %v", gbk, err)
	}
	raw, _ := encodeText(`a\n`, "UTF-8", true)
	if string(raw) != `a\n` {
		t.Fatalf("raw text was unescaped: %q", raw)
	}
}

func TestDebugSessionsPersist(t *testing.T) {
	path := debugSessionsPath(filepath.Join(t.TempDir(), "desktop.json"))
	sessions := []debugSession{
		{ID: "a", Kind: debugServer, Proto: debugUDP, Name: "logs", Port: 5514, AutoListen: true, Quick: []string{"ping"}},
		{ID: "b", Kind: debugClient, Proto: debugTCP, Name: "redis", Container: "redis", Port: 6379, Via: "net"},
		{ID: "c", Kind: "bogus", Proto: debugTCP},
	}
	if err := saveDebugSessions(path, sessions); err != nil {
		t.Fatal(err)
	}
	got := loadDebugSessions(path)
	if len(got) != 2 || got[0].Port != 5514 || !got[0].AutoListen || got[1].Container != "redis" || got[0].Quick[0] != "ping" {
		t.Fatalf("loaded %+v", got)
	}
	if loadDebugSessions(filepath.Join(t.TempDir(), "missing.json")) != nil {
		t.Fatal("a missing file is not empty")
	}
}

func TestMatchRoute(t *testing.T) {
	routes := []debugRoute{
		{Method: "POST", Path: "/v1/charges"},
		{Method: "GET", Path: "/v1/charges/:id"},
		{Method: "ANY", Path: "/static/*"},
	}
	for _, c := range []struct {
		method, path string
		want         int
		ok           bool
		param        string
	}{
		{"POST", "/v1/charges", 0, true, ""},
		{"GET", "/v1/charges", 0, false, ""},
		{"GET", "/v1/charges/ch_1", 1, true, "ch_1"},
		{"GET", "/v1/charges/ch_1/x", 0, false, ""},
		{"DELETE", "/static/a/b.css", 2, true, "a/b.css"},
	} {
		i, params, ok := matchRoute(routes, c.method, c.path)
		if ok != c.ok || ok && i != c.want {
			t.Errorf("%s %s = %d %v", c.method, c.path, i, ok)
		}
		if c.param != "" && params["id"] != c.param && params["*"] != c.param {
			t.Errorf("%s %s params %v", c.method, c.path, params)
		}
	}
	body := expandTemplate(`{"id":"{{path.id}}","n":"{{rand}}","u":"{{uuid}}","x":"{{other}}"}`, map[string]string{"id": "ch_1"})
	if !strings.Contains(body, `"id":"ch_1"`) || strings.Contains(body, "{{rand}}") || !strings.Contains(body, "{{other}}") {
		t.Fatalf("expandTemplate = %s", body)
	}
}

func TestTCPClientAndServer(t *testing.T) {
	port := freePort(t, "tcp")
	server := newDebugRun(nil)
	server.start(context.Background(), debugSession{Kind: debugServer, Proto: debugTCP, Port: port, Greeting: `hi\n`, AutoReply: true}, nil)
	defer server.stop()
	waitFor(t, server, "listening", func(s debugSnap) bool { return s.state == debugListening })

	client := newDebugRun(nil)
	client.start(context.Background(), debugSession{Kind: debugClient, Proto: debugTCP, Address: "127.0.0.1:" + strconv.Itoa(port)}, directDial)
	defer client.stop()
	waitFor(t, client, "greeting", received("hi\n"))
	if err := client.sendClient([]byte("PING"), false); err != nil {
		t.Fatal(err)
	}
	waitFor(t, client, "echo", received("PING"))
	snap := waitFor(t, server, "peer", func(s debugSnap) bool { return len(s.peers) == 1 })
	if !snap.peers[0].Picked {
		t.Fatal("the only peer is not picked")
	}
	if n, err := server.sendPeers([]byte("PUSH"), false); n != 1 || err != nil {
		t.Fatalf("sendPeers = %d, %v", n, err)
	}
	waitFor(t, client, "push", received("PUSH"))

	// Stopping the server ends the client's connection, and the port can
	// be listened on again at once.
	server.stop()
	waitFor(t, client, "close", func(s debugSnap) bool { return s.state == debugIdle })
	server.start(context.Background(), debugSession{Kind: debugServer, Proto: debugTCP, Port: port}, nil)
	waitFor(t, server, "listening again", func(s debugSnap) bool { return s.state == debugListening })
}

func TestTCPServerPortInUse(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	r := newDebugRun(nil)
	r.start(context.Background(), debugSession{Kind: debugServer, Proto: debugTCP, Port: ln.Addr().(*net.TCPAddr).Port}, nil)
	snap := waitFor(t, r, "failure", func(s debugSnap) bool { return s.state == debugFailed })
	if !strings.Contains(snap.problem, tr("端口已被占用", "The port is already in use")) {
		t.Fatalf("problem = %q", snap.problem)
	}
}

func TestUDPClientAndServer(t *testing.T) {
	port := freePort(t, "udp")
	server := newDebugRun(nil)
	server.start(context.Background(), debugSession{Kind: debugServer, Proto: debugUDP, Port: port, AutoReply: true, Reply: "ACK"}, nil)
	defer server.stop()
	waitFor(t, server, "listening", func(s debugSnap) bool { return s.state == debugListening })

	client := newDebugRun(nil)
	client.start(context.Background(), debugSession{Kind: debugClient, Proto: debugUDP, Address: "127.0.0.1:" + strconv.Itoa(port)}, directDial)
	defer client.stop()
	waitFor(t, client, "open", func(s debugSnap) bool { return s.state == debugOpen })
	if err := client.sendClient([]byte("metric:1|c"), false); err != nil {
		t.Fatal(err)
	}
	waitFor(t, client, "reply", received("ACK"))
	waitFor(t, server, "datagram", received("metric:1|c"))
}

func TestWebSocketClientAndServer(t *testing.T) {
	port := freePort(t, "tcp")
	server := newDebugRun(nil)
	server.start(context.Background(), debugSession{Kind: debugServer, Proto: debugWS, Port: port, AutoReply: true}, nil)
	defer server.stop()
	waitFor(t, server, "listening", func(s debugSnap) bool { return s.state == debugListening })

	client := newDebugRun(nil)
	client.start(context.Background(), debugSession{Kind: debugClient, Proto: debugWS, Address: "ws://127.0.0.1:" + strconv.Itoa(port) + "/live"}, directDial)
	defer client.stop()
	waitFor(t, client, "open", func(s debugSnap) bool { return s.state == debugOpen })
	if err := client.sendClient([]byte(`{"type":"ping"}`), false); err != nil {
		t.Fatal(err)
	}
	waitFor(t, client, "echo", received(`{"type":"ping"}`))
}

func TestWebSocketTLS(t *testing.T) {
	debugCertDir = t.TempDir()
	defer func() { debugCertDir = "" }()
	port := freePort(t, "tcp")
	server := newDebugRun(nil)
	server.start(context.Background(), debugSession{Kind: debugServer, Proto: debugWS, Port: port, TLS: true, AutoReply: true}, nil)
	defer server.stop()
	waitFor(t, server, "listening", func(s debugSnap) bool { return s.state == debugListening })
	// Certificates are checked by default, so the self-signed one fails.
	strict := newDebugRun(nil)
	strict.start(context.Background(), debugSession{Kind: debugClient, Proto: debugWS, Address: "wss://127.0.0.1:" + strconv.Itoa(port)}, directDial)
	defer strict.stop()
	waitFor(t, strict, "refusal", func(s debugSnap) bool { return s.state == debugFailed })
	client := newDebugRun(nil)
	client.start(context.Background(), debugSession{Kind: debugClient, Proto: debugWS, Insecure: true, Address: "wss://127.0.0.1:" + strconv.Itoa(port)}, directDial)
	defer client.stop()
	waitFor(t, client, "open", func(s debugSnap) bool { return s.state == debugOpen })
}

func TestMockAndHTTPClient(t *testing.T) {
	port := freePort(t, "tcp")
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("static"), 0o600); err != nil {
		t.Fatal(err)
	}
	mock := newDebugRun(nil)
	mock.start(context.Background(), debugSession{Kind: debugServer, Proto: debugHTTP, Port: port, Routes: []debugRoute{
		{Method: "POST", Path: "/v1/orders", Status: 201, ContentType: "application/json", Body: `{"id":"{{uuid}}"}`},
		{Method: "GET", Path: "/v1/orders/:id", Status: 200, Body: `{{path.id}}`},
		{Method: "ANY", Path: "/files/*", Dir: dir},
	}}, nil)
	defer mock.stop()
	waitFor(t, mock, "listening", func(s debugSnap) bool { return s.state == debugListening })
	base := "http://127.0.0.1:" + strconv.Itoa(port)

	client := newDebugRun(nil)
	client.doHTTP(context.Background(), debugSession{Kind: debugClient, Proto: debugHTTP, Address: base + "/v1/orders", Method: "POST",
		Headers: []debugHeader{{"Content-Type", "application/json"}}, Body: `{}`}, directDial)
	snap := client.snapshot()
	if len(snap.exchanges) != 1 || snap.exchanges[0].Status != 201 || !strings.Contains(string(snap.exchanges[0].Body), `"id":"`) {
		t.Fatalf("exchange %+v", snap.exchanges)
	}
	for path, want := range map[string]string{"/v1/orders/A-1": "A-1", "/files/a.txt": "static"} {
		resp, err := http.Get(base + path)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if string(body) != want {
			t.Errorf("GET %s = %q", path, body)
		}
	}
	resp, err := http.Get(base + "/missing")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Errorf("unmatched route answered %d", resp.StatusCode)
	}
	snap = waitFor(t, mock, "hits", func(s debugSnap) bool { return len(s.hits) == 4 })
	if snap.routeHits[0] != 1 || snap.routeHits[1] != 1 || snap.routeHits[2] != 1 {
		t.Fatalf("route hits %v", snap.routeHits)
	}
}

func TestLinesAreCapped(t *testing.T) {
	r := newDebugRun(nil)
	for i := range debugMaxLines + 10 {
		r.add(debugLine{Dir: lineNote, Note: strconv.Itoa(i)})
	}
	snap := r.snapshot()
	if len(snap.lines) > debugMaxLines || snap.lines[len(snap.lines)-1].Note != strconv.Itoa(debugMaxLines+9) {
		t.Fatalf("kept %d lines, last %q", len(snap.lines), snap.lines[len(snap.lines)-1].Note)
	}
}

func TestClientTargetKeepsPath(t *testing.T) {
	for _, c := range []struct {
		x    debugSession
		want string
	}{
		{debugSession{Proto: debugWS, Container: "web", Port: 8081, Address: "/live"}, "web:8081/live"},
		{debugSession{Proto: debugHTTP, Container: "api", Port: 3000, Address: "v1/orders?x=1"}, "api:3000/v1/orders?x=1"},
		{debugSession{Proto: debugTCP, Container: "db", Port: 5432, Address: "/ignored"}, "db:5432"},
		{debugSession{Proto: debugWS, Address: "ws://127.0.0.1:9001/a"}, "ws://127.0.0.1:9001/a"},
	} {
		if got := clientTarget(c.x); got != c.want {
			t.Errorf("clientTarget(%+v) = %q, want %q", c.x, got, c.want)
		}
	}
}

func TestServerListensOnEveryAddress(t *testing.T) {
	// 127.0.0.2 stands in for a bridge gateway; macOS has only 127.0.0.1
	// configured, so the test binds the IPv6 loopback instead when it can.
	extra := "::1"
	if ln, err := net.Listen("tcp", "[::1]:0"); err != nil {
		t.Skip("no IPv6 loopback")
	} else {
		ln.Close()
	}
	x := debugSession{Kind: debugServer, Proto: debugTCP, Port: freePort(t, "tcp"), AutoReply: true, Binds: []string{extra}}
	if got := x.listenAddresses(); len(got) != 2 || got[1] != net.JoinHostPort(extra, strconv.Itoa(x.Port)) {
		t.Fatalf("addresses %v", got)
	}
	r := newDebugRun(nil)
	r.start(context.Background(), x, nil)
	defer r.stop()
	waitFor(t, r, "listening", func(s debugSnap) bool { return s.state == debugListening })
	for _, addr := range x.listenAddresses() {
		conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
		if err != nil {
			t.Fatalf("%s: %v", addr, err)
		}
		_, _ = conn.Write([]byte("hi"))
		buf := make([]byte, 2)
		_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		if _, err := io.ReadFull(conn, buf); err != nil || string(buf) != "hi" {
			t.Fatalf("%s echoed %q, %v", addr, buf, err)
		}
		conn.Close()
	}
	// Every address being open, opening the session to every interface is
	// a single 0.0.0.0 binding.
	x.BindAll = true
	if got := x.listenAddresses(); len(got) != 1 || !strings.HasPrefix(got[0], "0.0.0.0:") {
		t.Fatalf("bind-all addresses %v", got)
	}
}

func TestDialContainerOldEngine(t *testing.T) {
	marked := true
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if marked {
			w.Header().Set("X-Fabo-Debug", "1")
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"message":"not found: no container named db"}`)
	}))
	defer server.Close()
	e := newEngineClient(func(ctx context.Context) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "tcp", strings.TrimPrefix(server.URL, "http://"))
	})
	_, err := e.dialContainer(context.Background(), "db", 5432, debugTCP)
	if err == nil || !strings.Contains(err.Error(), "no container named db") {
		t.Fatalf("missing container: %v", err)
	}
	marked = false
	_, err = e.dialContainer(context.Background(), "db", 5432, debugTCP)
	if err == nil || err.Error() != errOldEngine().Error() {
		t.Fatalf("old engine: %v", err)
	}
}
