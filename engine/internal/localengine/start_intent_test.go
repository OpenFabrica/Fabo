//go:build linux || darwin

package localengine

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/OpenFabrica/Fabo/engine/internal/guestwire"
)

// A container with a restart policy is on disk as starting, with its stop
// intent cleared, before the guest is asked to run it: a daemon crash before
// Started restores it as exited and the policy restarts it at boot. A --rm
// container has no policy and skips that write.
func TestStartRecordsIntentBeforeGuest(t *testing.T) {
	t.Run("restart policy", func(t *testing.T) { testStartRecordsIntent(t, false) })
	t.Run("rm", func(t *testing.T) { testStartRecordsIntent(t, true) })
}

func testStartRecordsIntent(t *testing.T, autoRemove bool) {
	dir := t.TempDir()
	e := &Engine{
		ctx:        t.Context(),
		images:     map[string]Image{"local": {Cmd: []string{"/bin/test"}}},
		containers: make(map[string]*container), state: &State{dir: dir},
	}
	cfg := Config{Image: "local", network: "none", autoRemove: autoRemove}
	if !autoRemove {
		cfg.restart = restartPolicy{Name: "unless-stopped"}
	}
	id, err := e.create("intent", cfg)
	if err != nil {
		t.Fatal(err)
	}
	c := e.containers[id]
	// Stopped by the user earlier: starting again must clear that on disk.
	c.state, c.manualStopped = "exited", true
	if err := e.save(c); err != nil {
		t.Fatal(err)
	}

	host, guest := net.Pipe()
	defer guest.Close()
	_ = guest.SetDeadline(time.Now().Add(3 * time.Second))
	e.session = guestwire.NewSession(host, e.onEvent)
	defer e.session.Close()
	started := make(chan error, 1)
	go func() { started <- e.start(id) }()

	// The guest has the Run request and has not answered: the crash window.
	frame, err := guestwire.ReadFrame(guest, nil)
	if err != nil || frame.Kind != guestwire.RunMessage {
		t.Fatalf("run frame: %+v, %v", frame, err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, id+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var rec savedContainer
	if err := json.Unmarshal(raw, &rec); err != nil {
		t.Fatal(err)
	}
	if autoRemove {
		if rec.State != "exited" || !rec.ManualStopped {
			t.Fatalf("--rm record rewritten before Started: %s, manually stopped %v", rec.State, rec.ManualStopped)
		}
	} else {
		if rec.State != "starting" || rec.ManualStopped {
			t.Fatalf("record before Started: %s, manually stopped %v", rec.State, rec.ManualStopped)
		}
		restored, err := (&State{dir: dir}).restore()
		if err != nil {
			t.Fatal(err)
		}
		if r := restored[id]; r == nil || !r.canRestart(true) {
			t.Fatalf("crash before Started loses the restart policy: %+v", r)
		}
	}

	// The guest goes away; the start fails and is recorded as such.
	_ = guest.Close()
	if err := <-started; err == nil {
		t.Fatal("start succeeded without a guest")
	}
}
