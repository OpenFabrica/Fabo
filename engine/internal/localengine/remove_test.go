//go:build linux || darwin

package localengine

import (
	"errors"
	"testing"

	"github.com/OpenFabrica/Fabo/engine/internal/guestwire"
)

func TestAutoRemoveNotifiesAlreadyRegisteredWaiter(t *testing.T) {
	for _, failure := range []error{nil, errors.New("start failed")} {
		c := &container{id: "id", state: "starting", attempt: &runAttempt{done: make(chan struct{})}, run: guestwire.RunSpec{Flags: guestwire.RunAutoRemove}}
		e := &Engine{containers: map[string]*container{c.id: c}}
		wait, err := e.prepareWait(c.id, "removed")
		if err != nil {
			t.Fatal(err)
		}
		e.mu.Lock()
		e.finish(c, 37, failure)
		e.mu.Unlock()
		code, err := wait(t.Context())
		if code != 37 || err != failure {
			t.Fatalf("removed wait: code=%d err=%v", code, err)
		}
		if _, ok := e.containers[c.id]; ok {
			t.Fatal("removed container still resolves")
		}
	}
}
