//go:build linux || darwin

package localengine

import (
	"strings"
	"testing"
	"time"

	"github.com/libracoder/kiri/engine/internal/guestwire"
)

func TestManualStopCancelsPendingRestartAndSurvivesRestore(t *testing.T) {
	id := strings.Repeat("a", 64)
	c := &container{id: id, name: "app", state: "exited", run: guestwire.RunSpec{ID: id},
		restartPolicy: restartPolicy{Name: "unless-stopped"}, restartCount: 2}
	e := &Engine{ctx: t.Context(), state: &State{dir: t.TempDir()}}
	e.mu.Lock()
	e.queueRestart(c, false, time.Hour)
	e.mu.Unlock()
	t.Cleanup(func() {
		e.mu.Lock()
		defer e.mu.Unlock()
		c.cancelRestart()
	})
	if err := e.markManualStop(c); err != nil {
		t.Fatal(err)
	}
	if c.restartTimer != nil || c.canRestart(false) {
		t.Fatal("manual stop left a restart pending")
	}
	records, err := e.state.restore()
	if err != nil {
		t.Fatal(err)
	}
	restored := records[id]
	if restored == nil || restored.restartPolicy != c.restartPolicy || restored.restartCount != 2 ||
		!restored.manualStopped || restored.canRestart(true) {
		t.Fatalf("unexpected restored restart state: %+v", restored)
	}
}
