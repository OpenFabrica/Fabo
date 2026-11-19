//go:build linux || darwin

package main

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

// fakeVersionEngine answers /version on a Unix socket with body, as an
// engine of some kind and version would.
func fakeVersionEngine(t *testing.T, body string) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "fabo-ver")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "s")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, body)
	})}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })
	return socket
}

// The engine is stale when it runs another Fabo release than this launcher;
// a development launcher and a foreign engine compare nothing. The release is
// FaboVersion; an engine older than that field reported it as Version, which
// is now the Docker Engine version.
func TestStaleEngine(t *testing.T) {
	saved := release
	t.Cleanup(func() { release = saved })
	current := func(fabo string) string {
		return fmt.Sprintf(`{"Version":"27.5.1","FaboVersion":%q,"FaboEngine":"go"}`, fabo)
	}
	older := func(fabo string) string { return fmt.Sprintf(`{"Version":%q,"FaboEngine":"go"}`, fabo) }
	for _, tt := range []struct {
		release, body, engine string
		stale                 bool
	}{
		{"0.5.0", current("0.5.0"), "0.5.0", false},
		{"0.5.0", current("0.0.0-perf-e14b3aa"), "0.0.0-perf-e14b3aa", true},
		{"0.5.0", current("0.4.0"), "0.4.0", true},
		{"0.0.0-dev", current("0.4.0"), "0.4.0", false},
		{"0.5.0", older("0.5.0"), "0.5.0", false},
		{"0.5.0", older("0.4.0"), "0.4.0", true},
		{"0.5.0", `{"Version":"27.0.1"}`, "", false},
	} {
		release = tt.release
		client := socketClient(fakeVersionEngine(t, tt.body))
		got := engineVersion(client)
		engine, stale := staleEngine(client)
		client.CloseIdleConnections()
		if got != tt.engine || stale != tt.stale || (stale && engine != tt.engine) {
			t.Errorf("launcher %s, engine %s: version %q, stale %v %q", tt.release, tt.body, got, stale, engine)
		}
	}
}
