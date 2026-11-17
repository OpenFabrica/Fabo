package main

import (
	"context"
	"encoding/json"
	"image/png"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/egoist/mygo/ui"
)

// fakeHost is fabo-win's control API, recording the actions it gets.
type fakeHost struct {
	mu      sync.Mutex
	status  hostStatus
	setup   hostSetup
	actions []string
}

// serve answers like engine/cmd/fabo-win/control.go.
func (f *fakeHost) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/status":
		json.NewEncoder(w).Encode(f.status)
	case r.Method == http.MethodGet && r.URL.Path == "/setup":
		json.NewEncoder(w).Encode(f.setup)
	case r.Method == http.MethodGet && r.URL.Path == "/update":
		json.NewEncoder(w).Encode(hostUpdate{State: "idle"})
	case r.Method == http.MethodPost:
		body, _ := io.ReadAll(r.Body)
		f.actions = append(f.actions, strings.TrimSpace(r.URL.Path+" "+string(body)))
		if r.URL.Path == "/setup/finish" {
			f.status.SetupPending = false
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		http.NotFound(w, r)
	}
}

// newSetupDesktop is a window whose host is a fakeHost at step.
func newSetupDesktop(t *testing.T, step string) (*desktop, *fakeHost) {
	t.Helper()
	f := &fakeHost{status: hostStatus{SetupPending: true}, setup: hostSetup{Step: step, MakeDefault: true, Autostart: true}}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	addr := strings.TrimPrefix(srv.URL, "http://")
	_, s := newFakeEngine(t)
	s.prefs.Lang = "en"
	s.host = newHostClient(func(ctx context.Context) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", addr)
	})
	s.refreshHost()
	return s, f
}

// TestSetupPageWalk drives the setup page's buttons for each step.
func TestSetupPageWalk(t *testing.T) {
	s, f := newSetupDesktop(t, "welcome")
	tt := ui.NewTester(s.view, 1100, 720)
	tt.Frame()
	if !tt.HasText("Run containers on Windows") {
		t.Fatal("welcome step not shown")
	}
	if err := tt.Click("Install WSL"); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.setup = hostSetup{Step: "choose", MakeDefault: true, ExistingUbuntu: "Ubuntu-24.04"}
	f.mu.Unlock()
	s.refreshHost()
	tt.Frame()
	if !tt.HasText("Ubuntu-24.04, already on this PC") {
		t.Fatal("existing Ubuntu not offered")
	}
	if err := tt.Click("Continue"); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	pct, done, total := int64(42), int64(150_000_000), int64(360_000_000)
	f.setup = hostSetup{Step: "engine", Busy: true, Progress: &hostProgress{Pct: &pct, Done: &done, Total: &total, Label: "Downloading"},
		Tasks: []hostTask{{"Start the engine", "done", "answered in 33 ms"}, {"Run a test container", "active", "pulling alpine"}},
		Log:   []string{"$ docker run --rm alpine echo hello"}}
	f.mu.Unlock()
	s.refreshHost()
	tt.Frame()
	if !tt.HasText("answered in 33 ms") || !tt.HasText("$ docker run --rm alpine echo hello") {
		t.Fatal("engine step's tasks or log not shown")
	}
	if err := tt.Click("Cancel"); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.setup = hostSetup{Step: "engine", Error: "couldn't pull alpine: i/o timeout"}
	f.mu.Unlock()
	s.refreshHost()
	tt.Frame()
	if !tt.HasText("couldn't pull alpine: i/o timeout") {
		t.Fatal("error not shown")
	}
	if err := tt.Click("Try again"); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.setup = hostSetup{Step: "ready", Autostart: true}
	f.mu.Unlock()
	s.refreshHost()
	tt.Frame()
	if err := tt.Click("Open Fabo"); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	if s.win.status.SetupPending {
		t.Fatal("setup still pending after finishing")
	}
	f.mu.Lock()
	got := strings.Join(f.actions, "\n")
	f.mu.Unlock()
	want := "/setup/install-wsl\n/setup/choose {\"value\":\"fabo\"}\n/setup/cancel\n/setup/retry\n/setup/finish"
	if got != want {
		t.Fatalf("actions:\n%s\nwant:\n%s", got, want)
	}
}

// TestHostEngineActions sends Start and Stop to fabo-win, not the engine.
func TestHostEngineActions(t *testing.T) {
	s, f := newSetupDesktop(t, "done")
	f.status.SetupPending = false
	s.do("engine", engineAction{http.MethodPost, "/fabo/wake", nil}, "")
	s.do("engine-stop", engineAction{http.MethodPost, "/fabo/stop", nil}, "")
	f.mu.Lock()
	defer f.mu.Unlock()
	if strings.Join(f.actions, ",") != "/engine/start,/engine/stop" {
		t.Fatalf("actions = %v", f.actions)
	}
}

// TestSetupScreenshots renders the setup steps (FABO_SHOTS=<dir>).
func TestSetupScreenshots(t *testing.T) {
	dir := os.Getenv("FABO_SHOTS")
	if dir == "" {
		t.Skip("set FABO_SHOTS to a directory to render the pages")
	}
	pct, done, total, eta := int64(58), int64(224_395_264), int64(367_575_040), int64(9)
	for _, theme := range []string{"dark", "light"} {
		for _, v := range []hostSetup{
			{Step: "welcome"},
			{Step: "choose", MakeDefault: true, OtherDocker: true},
			{Step: "wsl", Busy: true, Progress: &hostProgress{Pct: &pct, Done: &done, Total: &total, ETA: &eta, Label: "Downloading WSL 3.0.3 from Microsoft"},
				Tasks: []hostTask{{"Download WSL", "active", "224 / 368 MB"}, {"Administrator approval", "todo", ""}, {"Turn on Virtual Machine Platform", "todo", ""}, {"Install WSL", "todo", ""}}},
			{Step: "ready", Autostart: true},
		} {
			s, _ := newSetupDesktop(t, v.Step)
			s.prefs.Theme = theme
			s.win.setup = v
			tt := ui.NewTester(s.view, 1100, 720)
			tt.Frame()
			file, err := os.Create(filepath.Join(dir, "setup-"+v.Step+"-"+theme+".png"))
			if err != nil {
				t.Fatal(err)
			}
			png.Encode(file, tt.Image())
			file.Close()
		}
	}
}

// TestUpdateStrip offers a newer version and sends the download to fabo-win.
func TestUpdateStrip(t *testing.T) {
	s, f := newSetupDesktop(t, "done")
	f.mu.Lock()
	f.status.SetupPending = false
	f.mu.Unlock()
	s.refreshHost()
	s.win.update = hostUpdate{State: "available", Current: "0.5.2", Latest: "0.6.0"}
	tt := ui.NewTester(s.view, 1180, 760)
	tt.Frame()
	if !tt.HasText("Fabo 0.6.0 is available,") {
		t.Fatal("update strip not shown")
	}
	if err := tt.Click("Download"); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.actions) == 0 || f.actions[len(f.actions)-1] != "/update/download" {
		t.Fatalf("actions = %v", f.actions)
	}
}
