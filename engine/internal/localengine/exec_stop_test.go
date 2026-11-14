//go:build linux || darwin

package localengine

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/OpenFabrica/FabriBay/engine/internal/guestwire"
)

func TestDesktopExecStopKillsOnlyTheRequestedExec(t *testing.T) {
	c := &container{id: "target", state: "running"}
	c.nextAttempt()
	x := &execProcess{spec: guestwire.ExecSpec{ID: "session", ContainerID: c.id}, container: c, running: true, attempt: &runAttempt{done: make(chan struct{}), started: make(chan struct{})}}
	e := &Engine{ctx: t.Context(), containers: map[string]*container{c.id: c}, execs: map[string]*execProcess{x.spec.ID: x}}
	host, guest := net.Pipe()
	defer guest.Close()
	e.session = guestwire.NewSession(host, e.onEvent)
	defer e.session.Close()
	_ = guest.SetDeadline(time.Now().Add(3 * time.Second))
	sent := make(chan error, 1)
	go func() {
		frame, err := guestwire.ReadFrame(guest, nil)
		if err == nil {
			id, sig, decodeErr := guestwire.DecodeSignal(frame)
			if decodeErr != nil || id != x.spec.ID || sig != sigKill {
				t.Errorf("signal: %s %d %v", id, sig, decodeErr)
			}
			e.mu.Lock()
			e.finishExec(x, 137)
			e.mu.Unlock()
		}
		sent <- err
	}()
	r := httptest.NewRequest(http.MethodPost, "/fabo/exec/session/stop", nil)
	w := httptest.NewRecorder()
	e.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusNoContent {
		t.Fatalf("stop: %d %s", w.Code, w.Body.String())
	}
	if err := <-sent; err != nil {
		t.Fatal(err)
	}
	if c.state != "running" || x.running || x.exit == nil || *x.exit != 137 {
		t.Fatalf("container=%s exec running=%v exit=%v", c.state, x.running, x.exit)
	}
	// A finished exec stays inspectable, and repeated close is harmless.
	w = httptest.NewRecorder()
	e.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/fabo/exec/session/stop", nil))
	if w.Code != 204 {
		t.Fatal(w.Code)
	}
}
func TestDesktopExecStopRejectsPrivateAndCancelsAnUnstartedExec(t *testing.T) {
	c := &container{id: "target", state: "running"}
	e := &Engine{ctx: context.Background(), containers: map[string]*container{c.id: c}, execs: map[string]*execProcess{
		"queued":  {spec: guestwire.ExecSpec{ID: "queued"}, container: c},
		"private": {spec: guestwire.ExecSpec{ID: "private"}, container: c, internal: true},
	}}
	for id, want := range map[string]int{"unknown": 404, "private": 404, "queued": 204} {
		w := httptest.NewRecorder()
		e.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/fabo/exec/"+id+"/stop", nil))
		if w.Code != want {
			t.Errorf("%s: %d", id, w.Code)
		}
	}
	if e.execs["queued"] != nil {
		t.Fatal("cancelled exec could still start")
	}
}
