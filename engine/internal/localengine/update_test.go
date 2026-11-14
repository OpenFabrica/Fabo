//go:build linux || darwin

package localengine

import (
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/OpenFabrica/FabriBay/engine/internal/guestwire"
)

func TestUpdateRollsBackLiveLimitsWhenStateWriteFails(t *testing.T) {
	before := guestwire.Limits{MemoryMax: 64 << 20, PIDsMax: 50}
	c := &container{id: "id", name: "app", state: "running", run: guestwire.RunSpec{Limits: before}}
	c.nextAttempt()
	state := &State{dir: t.TempDir()}
	if err := os.Mkdir(filepath.Join(state.dir, c.id+".json"), 0700); err != nil {
		t.Fatal(err)
	}
	e := &Engine{ctx: t.Context(), state: state, containers: map[string]*container{c.id: c}}
	host, guest := net.Pipe()
	defer guest.Close()
	_ = guest.SetDeadline(time.Now().Add(3 * time.Second))
	e.session = guestwire.NewSession(host, e.onEvent)
	defer e.session.Close()
	result := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		w := httptest.NewRecorder()
		e.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/containers/app/update",
			strings.NewReader(`{"Memory":100663296}`)))
		result <- w
	}()
	for _, want := range []guestwire.Limits{{MemoryMax: 96 << 20, PIDsMax: 50}, before} {
		frame, err := guestwire.ReadFrame(guest, nil)
		if err != nil || frame.Kind != guestwire.UpdateMessage {
			t.Fatalf("update frame: %v, %v", frame.Kind, err)
		}
		if id, got, err := guestwire.DecodeUpdate(frame); err != nil || id != c.id || got != want {
			t.Fatalf("update to %q: %+v, %v, want %+v", id, got, err, want)
		}
		if err := guestwire.WriteFrame(guest, guestwire.Frame{Kind: guestwire.AckMessage}); err != nil {
			t.Fatal(err)
		}
	}
	w := <-result
	if w.Code != http.StatusInternalServerError || !strings.Contains(w.Body.String(), "save container") || c.run.Limits != before {
		t.Fatalf("failed update: HTTP %d %s, limits %+v", w.Code, w.Body, c.run.Limits)
	}
}
