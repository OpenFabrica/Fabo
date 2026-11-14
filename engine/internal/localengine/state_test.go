//go:build linux || darwin

package localengine

import (
	"os"
	"strings"
	"testing"

	"github.com/OpenFabrica/FabriBay/engine/internal/guestwire"
)

func TestStateRestoresCreatedAndExitedContainers(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	store, err := OpenState(dir)
	if err != nil {
		t.Fatal(err)
	}
	if second, err := OpenState(dir); err == nil {
		_ = second.Close()
		t.Fatal("second writer acquired the same state directory")
	}
	e := &Engine{state: store, images: map[string]Image{"local": {Cmd: []string{"/bin/test"}}}, containers: map[string]*container{}}
	created, err := e.create("created", Config{Image: "local"})
	if err != nil {
		t.Fatal(err)
	}
	exited, err := e.create("exited", Config{Image: "local"})
	if err != nil {
		t.Fatal(err)
	}
	running, err := e.create("running", Config{Image: "local"})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{exited, running} {
		c := e.containers[id]
		c.state = "starting"
		c.nextAttempt()
		if err := store.save(c); err != nil {
			t.Fatal(err)
		}
		e.onEvent(guestwire.RunEvent{Kind: guestwire.StartedMessage, ID: id, PID: 10})
	}
	exitedStart, interruptedStart := e.containers[exited].startedAt, e.containers[running].startedAt
	e.containers[exited].appendLog(record{stream: 1, data: []byte("old stdout")})
	e.onEvent(guestwire.RunEvent{Kind: guestwire.OutputMessage, ID: exited, Stream: 2, Data: []byte("stderr")})
	stamp := e.containers[exited].logs[1].at
	e.onEvent(guestwire.RunEvent{Kind: guestwire.ExitedMessage, ID: exited, Exit: guestwire.Exit{Code: 37}})
	exitedFinish := e.containers[exited].finishedAt
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = OpenState(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	restored, err := store.restore()
	if err != nil {
		t.Fatal(err)
	}
	if c := restored[created]; c == nil || c.state != "created" || c.run.Argv[0] != "/bin/test" || !c.startedAt.IsZero() || !c.finishedAt.IsZero() || c.vmStopped {
		t.Fatalf("created record: %+v", c)
	}
	if c := restored[exited]; c == nil || c.state != "exited" || c.exit != 37 || c.vmStopped ||
		!c.startedAt.Equal(exitedStart) || !c.finishedAt.Equal(exitedFinish) || c.finishedAt.Before(c.startedAt) || len(c.logs) != 2 ||
		!c.logs[0].at.IsZero() || string(c.logs[0].data) != "old stdout" ||
		c.logs[1].stream != 2 || string(c.logs[1].data) != "stderr" || stamp.IsZero() || !c.logs[1].at.Equal(stamp) {
		t.Fatalf("exited record: %+v", c)
	}
	if c := restored[running]; c == nil || c.state != "exited" || c.exit != 255 || !c.vmStopped ||
		interruptedStart.IsZero() || !c.startedAt.Equal(interruptedStart) || c.finishedAt.Before(c.startedAt) ||
		c.failure == nil || !strings.Contains(c.failure.Error(), "daemon stopped") {
		t.Fatalf("interrupted record: %+v", c)
	}
}

func TestRestoreRetainsLegacyNoNewPrivileges(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	state, err := OpenState(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()

	legacyID, currentID := strings.Repeat("a", 64), strings.Repeat("b", 64)
	legacy := savedContainer{
		ID: legacyID, Name: "legacy", State: "created",
		Run: guestwire.RunSpec{ID: legacyID},
	}
	if err := saveJSON(state.dir, legacyID+".json", legacy); err != nil {
		t.Fatal(err)
	}
	current := &container{
		id: currentID, name: "current", state: "created",
		run: guestwire.RunSpec{ID: currentID},
	}
	if err := state.save(current); err != nil {
		t.Fatal(err)
	}

	restored, err := state.restore()
	if err != nil {
		t.Fatal(err)
	}
	if restored[legacyID].run.Flags&guestwire.RunNoNewPrivs == 0 {
		t.Fatal("legacy record lost its no-new-privileges policy")
	}
	if restored[currentID].run.Flags&guestwire.RunNoNewPrivs != 0 {
		t.Fatal("new record inherited the legacy no-new-privileges policy")
	}
}
