//go:build linux || darwin

package localengine

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/OpenFabrica/Fabo/engine/internal/guestwire"
)

// A container can share another's PID namespace by name or short ID: the
// record keeps the full ID across a restore, inspect reports it, and the
// container cannot start while that other one is not running.
func TestSharedPIDNamespaceKeepsCanonicalTarget(t *testing.T) {
	e := &Engine{
		images:     map[string]Image{"local": {Cmd: []string{"/bin/test"}}},
		containers: make(map[string]*container), state: &State{dir: t.TempDir()},
	}
	owner, err := e.create("owner", Config{Image: "local", network: "bridge"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.create("missing", Config{Image: "local", pidMode: "nope"}); !errors.Is(err, errMissing) {
		t.Fatalf("missing PID namespace target: %v", err)
	}
	id, err := e.create("debugger", Config{Image: "local", pidMode: owner[:12], network: "container:owner"})
	if err != nil {
		t.Fatal(err)
	}
	e.containers, err = e.state.restore()
	if err != nil {
		t.Fatal(err)
	}
	c := e.containers[id]
	if c.pidMode() != "container:"+owner || c.pidTarget() != owner {
		t.Fatalf("restored PID mode %q from %q", c.pidMode(), c.run.Options)
	}
	if target, rest := guestwire.SplitPIDJoin(c.run.Options); target != owner || len(rest) != len(c.run.Options)-1 {
		t.Fatalf("split %q %q", target, rest)
	}
	c.setEndpoints(c.endpoints())
	c.nextAttempt()
	if err := e.startContainer(c, false); !errors.Is(err, errConflict) || !strings.Contains(err.Error(), "network") && !strings.Contains(err.Error(), "PID namespace") {
		t.Fatalf("starting without a running target: %v", err)
	}
	// Without a shared network, the PID check alone stops the start.
	alone, err := e.create("alone", Config{Image: "local", pidMode: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	c = e.containers[alone]
	c.nextAttempt()
	if err := e.startContainer(c, false); !errors.Is(err, errConflict) || !strings.Contains(err.Error(), "cannot join PID namespace of a non running container") {
		t.Fatalf("PID target not running: %v", err)
	}
}

// Only another container's PID namespace can be asked for.
func TestPIDModeAcceptsOnlyContainers(t *testing.T) {
	e := &Engine{
		images:     map[string]Image{"local": {Cmd: []string{"/bin/test"}}},
		containers: make(map[string]*container), state: &State{dir: t.TempDir()},
	}
	for _, mode := range []string{"host", "container:", "private"} {
		r := httptest.NewRequest(http.MethodPost, "/containers/create", strings.NewReader(`{"Image":"local","HostConfig":{"PidMode":"`+mode+`"}}`))
		w := httptest.NewRecorder()
		e.Handler().ServeHTTP(w, r)
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "PID mode") {
			t.Fatalf("PidMode %q: %d %s", mode, w.Code, w.Body)
		}
	}
}
