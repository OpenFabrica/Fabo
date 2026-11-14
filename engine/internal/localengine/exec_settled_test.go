//go:build linux || darwin

package localengine

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/OpenFabrica/Fabo/engine/internal/guestwire"
)

// An exec started while its container is still setting up waits for the
// guest's Settled event before asking the guest, and waits holding no
// container lock: a CONT for a container stalled in its setup still gets
// through. A run that ends meanwhile rejects the exec.
func TestExecWaitsForSettled(t *testing.T) {
	t.Run("settled", func(t *testing.T) { testExecWaitsForSettled(t, false) })
	t.Run("exited", func(t *testing.T) { testExecWaitsForSettled(t, true) })
}

func testExecWaitsForSettled(t *testing.T, exits bool) {
	c := &container{id: "target", state: "running"}
	c.nextAttempt().settled = make(chan struct{})
	e := &Engine{ctx: t.Context(), containers: map[string]*container{c.id: c}}
	host, guest := net.Pipe()
	_ = guest.SetDeadline(time.Now().Add(5 * time.Second))
	e.session = guestwire.NewSession(host, e.onEvent)
	server := httptest.NewServer(e.Handler())
	// The guest goes first, so a failing test cannot leave a handler
	// waiting for its reply while the server closes.
	defer func() {
		_ = guest.Close()
		e.session.Close()
		server.Close()
	}()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	request := func(method, path, body string) *http.Response {
		t.Helper()
		req, err := http.NewRequestWithContext(ctx, method, server.URL+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		res, err := server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	frames := make(chan guestwire.Frame, 8)
	go func() {
		for {
			frame, err := guestwire.ReadFrame(guest, nil)
			if err != nil {
				close(frames)
				return
			}
			frames <- frame
		}
	}()

	res := request(http.MethodPost, "/containers/target/exec", `{"Cmd":["/bin/test"]}`)
	var created struct{ ID string }
	if err := json.NewDecoder(res.Body).Decode(&created); res.StatusCode != http.StatusCreated || err != nil {
		t.Fatalf("create: %s, %v", res.Status, err)
	}
	res.Body.Close()
	started := make(chan *http.Response, 1)
	go func() { started <- request(http.MethodPost, "/exec/"+created.ID+"/start", `{"Detach":true}`) }()

	// Waiting: nothing reaches the guest, and the container lock is free.
	select {
	case frame := <-frames:
		t.Fatalf("guest asked before Settled: kind %d", frame.Kind)
	case <-time.After(100 * time.Millisecond):
	}
	if !c.operation.TryLock() {
		t.Fatal("exec holds the container's operation lock while waiting")
	}
	c.operation.Unlock()
	go func() {
		res := request(http.MethodPost, "/containers/target/kill?signal=CONT", "")
		_, _ = io.Copy(io.Discard, res.Body)
		res.Body.Close()
	}()
	if frame := <-frames; frame.Kind != guestwire.KillMessage {
		t.Fatalf("CONT did not reach the guest: kind %d", frame.Kind)
	}

	if exits {
		e.onEvent(guestwire.RunEvent{Kind: guestwire.ExitedMessage, ID: c.id, Exit: guestwire.Exit{Code: 137}})
		res := <-started
		res.Body.Close()
		if res.StatusCode != http.StatusConflict {
			t.Fatalf("exec into an ended run: %s", res.Status)
		}
		return
	}
	e.onEvent(guestwire.RunEvent{Kind: guestwire.SettledMessage, ID: c.id})
	if frame := <-frames; frame.Kind != guestwire.ExecMessage {
		t.Fatalf("exec not sent after Settled: kind %d", frame.Kind)
	}
	reply, err := guestwire.RunEvent{Kind: guestwire.StartedMessage, ID: created.ID, PID: 9}.Frame()
	if err != nil {
		t.Fatal(err)
	}
	if err := guestwire.WriteFrame(guest, reply); err != nil {
		t.Fatal(err)
	}
	res = <-started
	res.Body.Close()
	// A detached start answers 204, as Docker does.
	if res.StatusCode != http.StatusNoContent {
		t.Fatalf("exec start: %s", res.Status)
	}
}
