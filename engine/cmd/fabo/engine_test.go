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

// fakeVersionEngine answers /version on a Unix socket as engine `version`
// of kind `kind` would.
func fakeVersionEngine(t *testing.T, kind, version string) string {
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
		fmt.Fprintf(w, `{"Version":%q,"FaboEngine":%q}`, version, kind)
	})}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })
	return socket
}

// The engine is stale when it runs another version than this launcher; a
// development launcher and a foreign engine compare nothing.
func TestStaleEngine(t *testing.T) {
	saved := release
	t.Cleanup(func() { release = saved })
	for _, tt := range []struct {
		release, kind, engine string
		stale                 bool
	}{
		{"0.5.0", "go", "0.5.0", false},
		{"0.5.0", "go", "0.0.0-perf-e14b3aa", true},
		{"0.5.0", "go", "0.4.0", true},
		{"0.0.0-dev", "go", "0.4.0", false},
		{"0.5.0", "", "27.0.1", false},
	} {
		release = tt.release
		client := socketClient(fakeVersionEngine(t, tt.kind, tt.engine))
		engine, stale := staleEngine(client)
		client.CloseIdleConnections()
		if stale != tt.stale || (stale && engine != tt.engine) {
			t.Errorf("launcher %s, engine %s %q: stale %v %q", tt.release, tt.kind, tt.engine, stale, engine)
		}
	}
}
