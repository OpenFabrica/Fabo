//go:build linux || darwin

package localengine

import (
	"testing"

	"github.com/OpenFabrica/Fabo/engine/internal/guestwire"
)

// A guest memory-pressure notice names no container: it goes to the
// MemoryPressure callback, and without one it is dropped.
func TestMemoryPressureReachesTheCallback(t *testing.T) {
	calls := 0
	e := &Engine{containers: map[string]*container{}, memoryPressure: func() { calls++ }}
	e.onEvent(guestwire.RunEvent{Kind: guestwire.MemoryPressure})
	e.onEvent(guestwire.RunEvent{Kind: guestwire.MemoryPressure})
	if calls != 2 {
		t.Fatalf("callback ran %d times, want 2", calls)
	}
	(&Engine{containers: map[string]*container{}}).onEvent(guestwire.RunEvent{Kind: guestwire.MemoryPressure})
}
