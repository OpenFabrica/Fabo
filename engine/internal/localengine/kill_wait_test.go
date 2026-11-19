//go:build linux || darwin

package localengine

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/OpenFabrica/Fabo/engine/internal/guestwire"
)

// A SIGKILL is answered only once the container has exited, as Docker
// answers it, so a start or rm right after kill finds it stopped; an exit
// the guest never reports gives up after killWait. Other signals answer at
// once.
func TestKillWaitsForTheExit(t *testing.T) {
	t.Run("exits", func(t *testing.T) { testKillWait(t, "", true, http.StatusNoContent) })
	t.Run("never exits", func(t *testing.T) { testKillWait(t, "", false, http.StatusInternalServerError) })
	t.Run("other signal", func(t *testing.T) { testKillWait(t, "?signal=HUP", false, http.StatusNoContent) })
}

// testKillWait kills a running container through the API and reports the
// exit (or not) after checking that a SIGKILL answer waited for it.
func testKillWait(t *testing.T, query string, exits bool, want int) {
	old := killWait
	killWait = 300 * time.Millisecond
	defer func() { killWait = old }()
	c := &container{id: "target", state: "running"}
	c.nextAttempt()
	e := &Engine{ctx: t.Context(), containers: map[string]*container{c.id: c}}
	host, guest := net.Pipe()
	_ = guest.SetDeadline(time.Now().Add(5 * time.Second))
	e.session = guestwire.NewSession(host, e.onEvent)
	server := httptest.NewServer(e.Handler())
	defer func() {
		_ = guest.Close()
		e.session.Close()
		server.Close()
	}()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
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
	answered := make(chan int, 1)
	go func() {
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/containers/target/kill"+query, nil)
		res, err := server.Client().Do(req)
		if err != nil {
			answered <- 0
			return
		}
		_, _ = io.Copy(io.Discard, res.Body)
		res.Body.Close()
		answered <- res.StatusCode
	}()
	if frame := <-frames; frame.Kind != guestwire.KillMessage {
		t.Fatalf("the signal did not reach the guest: kind %d", frame.Kind)
	}
	if query == "" && exits {
		select {
		case code := <-answered:
			t.Fatalf("kill answered %d before the container exited", code)
		case <-time.After(100 * time.Millisecond):
		}
		e.onEvent(guestwire.RunEvent{Kind: guestwire.ExitedMessage, ID: c.id, Exit: guestwire.Exit{Code: 137}})
	}
	if got := <-answered; got != want {
		t.Fatalf("kill answered %d, want %d", got, want)
	}
}
