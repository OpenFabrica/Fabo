//go:build linux || darwin

package localengine

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/OpenFabrica/FabriBay/engine/internal/guestwire"
)

func TestTopRejectsReplyAfterRunExit(t *testing.T) {
	c := &container{id: "id", name: "app", state: "running"}
	c.nextAttempt()
	e := &Engine{containers: map[string]*container{c.id: c}}
	host, guest := net.Pipe()
	defer guest.Close()
	_ = guest.SetDeadline(time.Now().Add(3 * time.Second))
	e.session = guestwire.NewSession(host, e.onEvent)
	defer e.session.Close()

	responses := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		w := httptest.NewRecorder()
		e.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/containers/app/top", nil))
		responses <- w
	}()
	frame, err := guestwire.ReadFrame(guest, nil)
	if err != nil || frame.Kind != guestwire.TopQueryMessage {
		t.Fatalf("top query: %v, %v", frame.Kind, err)
	}
	if id, err := guestwire.DecodeContainerQuery(frame); err != nil || id != c.id {
		t.Fatalf("top target: %q, %v", id, err)
	}
	exited, err := (guestwire.RunEvent{Kind: guestwire.ExitedMessage, ID: c.id}).Frame()
	if err != nil {
		t.Fatal(err)
	}
	if err := guestwire.WriteFrame(guest, exited); err != nil {
		t.Fatal(err)
	}
	top, err := guestwire.TopFrame([]string{"root\t5\t1\t0\t01:00\t?\t00:00:00\told process"})
	if err != nil {
		t.Fatal(err)
	}
	if err := guestwire.WriteFrame(guest, top); err != nil {
		t.Fatal(err)
	}
	w := <-responses
	if w.Code != http.StatusConflict || strings.Contains(w.Body.String(), "old process") {
		t.Fatalf("late process list: HTTP %d %s", w.Code, w.Body)
	}
}
