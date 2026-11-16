//go:build linux || darwin

package main

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/OpenFabrica/Fabo/engine/sharetunnel"
)

func TestParseShareArgs(t *testing.T) {
	ok := map[string]shareOptions{
		"3000":                             {target: "3000"},
		"3000 myapp":                       {target: "3000", name: "myapp"},
		"web --name site --no-reserve":     {target: "web", name: "site", noReserve: true},
		"localhost:8080 --name=api --json": {target: "localhost:8080", name: "api", json: true},
	}
	for in, want := range ok {
		got, err := parseShareArgs(strings.Fields(in))
		if err != nil || got != want {
			t.Errorf("%q: %+v %v", in, got, err)
		}
	}
	for _, in := range []string{"", "3000 a b", "3000 a --name b", "--json", "3000 --bogus", "3000 --name"} {
		if _, err := parseShareArgs(strings.Fields(in)); err == nil {
			t.Errorf("%q should be rejected", in)
		}
	}
}

// The `fabo PORT` shorthand must never take a docker or Fabo command.
func TestShareShorthandLeavesCommandsAlone(t *testing.T) {
	commands := strings.Fields(`attach build ps run exec compose logs ui daemon setup share usb k8s machine import pack debug raw bench mcp engine context migrate domains build-with`)
	for _, c := range commands {
		if sharetunnel.IsShorthand(c) {
			t.Errorf("%s would be taken as a port", c)
		}
	}
	for _, port := range []string{"3000", "8080", "localhost:5173", "api.internal:80"} {
		if !sharetunnel.IsShorthand(port) || handlesDocker(port) {
			t.Errorf("%s should be the share shorthand", port)
		}
	}
}

// syncBuffer is a Buffer safe to read while runShare writes.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func waitOutput(t *testing.T, out *syncBuffer, want string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(out.String(), want) {
		if time.Now().After(deadline) {
			t.Fatalf("no %q in:\n%s", want, out.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestRequestLineAndSizes(t *testing.T) {
	line := requestLine(sharetunnel.Request{Method: "GET", Path: "/" + strings.Repeat("a", 60), Status: 200, MS: 12, Out: 2048, At: time.Date(2026, 1, 2, 3, 4, 5, 0, time.Local)})
	if !strings.HasPrefix(line, "03:04:05 GET    200 /aaa") || !strings.Contains(line, "…") || !strings.HasSuffix(line, "2.0 KB") {
		t.Fatalf("line %q", line)
	}
	for n, want := range map[uint64]string{0: "0 B", 1023: "1023 B", 1 << 20: "1.0 MB", 3 << 30: "3.0 GB"} {
		if got := humanBytes(n); got != want {
			t.Errorf("%d: %s", n, got)
		}
	}
}

// TestShareBillingCommands shows billing details and opens only the URL returned by the daemon.
func TestShareBillingCommands(t *testing.T) {
	var opened string
	saved := openURL
	openURL = func(link string) error { opened = link; return nil }
	defer func() { openURL = saved }()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/fabo/share":
			io.WriteString(w, `{"account":{"signedIn":true,"server":"edge.test","login":"a"},"shares":[{"target":"3000","state":"waiting","reason":"quota exhausted: traffic"}]}`)
		case "/fabo/share/account":
			io.WriteString(w, `{"billing":true,"plan":{"id":"pro","name":"Pro","expires":"2027-01-01T00:00:00+08:00"},"usage":{"used":1073741824,"cap":5368709120,"resets":"2026-12-01T00:00:00+08:00"}}`)
		case "/fabo/share/billing":
			if r.Method != "POST" {
				t.Error("wrong billing method")
			}
			io.WriteString(w, `{"url":"https://edge.test/account?t=once"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	c := &commandClient{http: &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
	}}}}
	var out bytes.Buffer
	if err := runShare(context.Background(), c, []string{"status"}, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Pro (pro)", "traffic", "2026-12-01", "2027-01-01", "waiting for quota"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("missing %s: %s", want, out.String())
		}
	}
	out.Reset()
	if err := runShare(context.Background(), c, []string{"status", "--json"}, &out, io.Discard); err != nil || !strings.Contains(out.String(), `"billing":true`) {
		t.Fatalf("JSON: %s %v", out.String(), err)
	}
	out.Reset()
	if err := runShare(context.Background(), c, []string{"upgrade"}, &out, io.Discard); err != nil || opened != "https://edge.test/account?t=once" || strings.TrimSpace(out.String()) != opened {
		t.Fatalf("upgrade: %s %s %v", opened, out.String(), err)
	}
}

// TestShareWaitingKeepsCLIStreamAlive verifies waiting is reported without ending the daemon lease.
func TestShareWaitingKeepsCLIStreamAlive(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		io.WriteString(w, "{\"type\":\"waiting\",\"reason\":\"quota exhausted: traffic\"}\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer server.Close()
	c := &commandClient{http: &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
	}}}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := &syncBuffer{}
	done := make(chan error, 1)
	go func() { done <- runShare(ctx, c, []string{"3000"}, out, io.Discard) }()
	waitOutput(t, out, "waiting for quota")
	select {
	case err := <-done:
		t.Fatalf("waiting ended CLI: %v", err)
	default:
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancel did not end CLI")
	}
}
