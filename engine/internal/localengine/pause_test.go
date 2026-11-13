//go:build linux || darwin

package localengine

import (
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/libracoder/kiri/engine/internal/guestwire"
)

func TestPauseReportsOnlyAcknowledgedChanges(t *testing.T) {
	c := &container{id: "running", state: "running"}
	c.nextAttempt()
	e := &Engine{containers: map[string]*container{c.id: c}}
	host, guest := net.Pipe()
	defer guest.Close()
	_ = guest.SetDeadline(time.Now().Add(3 * time.Second))
	e.session = guestwire.NewSession(host, e.onEvent)
	defer e.session.Close()
	handler := e.Handler()

	request := func(action string) <-chan int {
		status := make(chan int, 1)
		go func() {
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/containers/running/"+action, nil))
			status <- response.Code
		}()
		return status
	}
	receive := func(freeze bool) {
		t.Helper()
		frame, err := guestwire.ReadFrame(guest, nil)
		if err != nil || frame.Kind != guestwire.FreezeMessage {
			t.Fatalf("freeze frame: %+v, %v", frame, err)
		}
		id, value, err := guestwire.DecodeFreeze(frame)
		if err != nil || id != c.id || value != freeze {
			t.Fatalf("freeze %q %v: %v", id, value, err)
		}
	}
	paused := func() bool {
		e.mu.Lock()
		defer e.mu.Unlock()
		return c.paused
	}

	status := request("pause")
	receive(true)
	if paused() {
		t.Fatal("container reported paused before the guest replied")
	}
	if err := guestwire.WriteFrame(guest, guestwire.Frame{Kind: guestwire.AckMessage}); err != nil {
		t.Fatal(err)
	}
	if code := <-status; code != http.StatusNoContent || !paused() {
		t.Fatalf("acknowledged pause: HTTP %d, paused %t", code, paused())
	}

	status = request("unpause")
	receive(false)
	if err := guestwire.WriteFrame(guest, guestwire.Frame{Kind: guestwire.ErrorMessage, Payload: []byte("thaw failed")}); err != nil {
		t.Fatal(err)
	}
	if code := <-status; code != http.StatusInternalServerError || !paused() {
		t.Fatalf("rejected resume: HTTP %d, paused %t", code, paused())
	}
}
