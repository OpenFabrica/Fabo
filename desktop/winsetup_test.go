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
	build   hostBuildWith
	actions []string
	// resources is GET /resources' answer.
	resources vmResources
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
	case r.Method == http.MethodGet && r.URL.Path == "/buildwith":
		json.NewEncoder(w).Encode(f.build)
	case r.Method == http.MethodGet && r.URL.Path == "/update":
		json.NewEncoder(w).Encode(hostUpdate{State: "idle"})
	case r.Method == http.MethodGet && r.URL.Path == "/resources":
		json.NewEncoder(w).Encode(f.resources)
	case r.Method == http.MethodPost && r.URL.Path == "/diag/report":
		body, _ := io.ReadAll(r.Body)
		f.actions = append(f.actions, strings.TrimSpace(r.URL.Path+" "+string(body)))
		json.NewEncoder(w).Encode(map[string]string{"path": `~\Desktop\Fabo report.txt`})
	case r.Method == http.MethodPost:
		body, _ := io.ReadAll(r.Body)
		f.actions = append(f.actions, strings.TrimSpace(r.URL.Path+" "+string(body)))
		if r.URL.Path == "/setup/finish" {
			f.status.SetupPending = false
		}
		if r.URL.Path == "/buildwith" {
			var v struct{ Value string }
			json.Unmarshal(body, &v)
			f.build.With, f.build.Ask, f.build.Taken = v.Value, false, v.Value == "fabo"
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

// TestHostProxyRefresh: saving proxy settings on Windows has fabo-win
// push them at once, so a custom proxy on Windows' loopback gets the relay.
func TestHostProxyRefresh(t *testing.T) {
	s, f := newSetupDesktop(t, "done")
	f.status.SetupPending = false
	s.do("proxy", engineAction{http.MethodPost, "/fabo/proxy", proxySettings{Mode: "custom", HTTP: "http://127.0.0.1:7890"}}, "")
	f.mu.Lock()
	defer f.mu.Unlock()
	if strings.Join(f.actions, ",") != "/proxy" {
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

// TestBuildWithOnWindows: beside another engine the window asks once which
// should build, and Settings → Builds switches it.
func TestBuildWithOnWindows(t *testing.T) {
	s, f := newSetupDesktop(t, "done")
	f.mu.Lock()
	f.status.SetupPending = false
	f.build = hostBuildWith{With: "docker", Ask: true, Engine: "Docker Desktop", Ready: true, Builder: "fabo-build"}
	f.mu.Unlock()
	s.refreshHost()
	tt := ui.NewTester(s.view, 1100, 720)
	tt.Frame()
	if !tt.HasText("You already have Docker Desktop. Which should build your images?") {
		t.Fatal("not asked beside another engine")
	}
	if err := tt.Click("Build with Fabo"); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	if tt.HasText("Which should build your images?") || s.win.build.With != "fabo" {
		t.Fatalf("the question stays, or the choice was lost: %+v", s.win.build)
	}
	s.navigate(settingsPage)
	s.settingsTab = indexSettings("builds")
	s.refreshHost()
	tt.Frame()
	if !tt.HasText("where Docker Desktop doesn't see them") {
		t.Fatal("Settings → Builds does not show building with Fabo")
	}
	if err := tt.Click("Docker"); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	f.mu.Lock()
	got := strings.Join(f.actions, "\n")
	f.mu.Unlock()
	if !strings.Contains(got, `/buildwith {"value":"fabo"}`) || !strings.Contains(got, `/buildwith {"value":"docker"}`) {
		t.Fatalf("actions = %s", got)
	}
	if !tt.HasText("Docker Desktop's builds stay there") {
		t.Fatal("not switched back to Docker")
	}
	// Escape on the question keeps Docker.
	s2, f2 := newSetupDesktop(t, "done")
	f2.mu.Lock()
	f2.status.SetupPending = false
	f2.build = hostBuildWith{With: "docker", Ask: true, Engine: "Podman", Ready: true}
	f2.mu.Unlock()
	s2.refreshHost()
	tt2 := ui.NewTester(s2.view, 1100, 720)
	tt2.Frame()
	tt2.Key(0, ui.KeyEscape)
	tt2.Frame()
	f2.mu.Lock()
	defer f2.mu.Unlock()
	if len(f2.actions) != 1 || f2.actions[0] != `/buildwith {"value":"docker"}` {
		t.Fatalf("escape: %v", f2.actions)
	}
}

// TestWindowsResourcesAndDiagnostics: on Windows, Settings → Resources
// sizes WSL through fabo-win's .wslconfig endpoints, and Diagnostics makes
// a report there; the Diagnostics section is not offered elsewhere.
func TestWindowsResourcesAndDiagnostics(t *testing.T) {
	s, f := newSetupDesktop(t, "done")
	f.mu.Lock()
	f.status.SetupPending = false
	f.resources = vmResources{CPUs: 4, Memory: 8192, RunningCPUs: 4, RunningMemory: 8192, Awake: true, MaxCPUs: 8, MaxMemory: 16384}
	f.mu.Unlock()
	s.refreshHost()
	s.navigate(settingsPage)
	s.settingsTab = indexSettings("resources")
	tt := ui.NewTester(s.view, 1100, 720)
	tt.Frame()
	tt.Frame()
	if !tt.HasText("Running now with 4 CPUs and up to 8 GB.") || !tt.HasText("shared by every WSL distro") {
		t.Fatal("WSL resources not shown")
	}
	s.cpus = 6
	tt.Frame()
	if err := tt.Click("Save"); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	got := strings.Join(f.actions, "\n")
	// The saved values are what the VM does not run with yet: restart offered.
	f.resources.CPUs = 6
	f.mu.Unlock()
	if !strings.Contains(got, `/resources {"cpus":6,"memory_mib":8192}`) {
		t.Fatalf("actions = %s", got)
	}
	s.loadSettings()
	tt.Frame()
	if err := tt.Click("Restart WSL to apply"); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	if !tt.HasText("every WSL distro shuts down") {
		t.Fatal("restart not confirmed with its warning")
	}
	if err := tt.Click("Restart"); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	f.mu.Lock()
	got = strings.Join(f.actions, "\n")
	f.mu.Unlock()
	if !strings.Contains(got, "/resources/restart") {
		t.Fatalf("restart not sent: %s", got)
	}

	s.pending = nil
	s.settingsTab = indexSettings("diagnostics")
	tt.Frame()
	if err := tt.Click("Make report"); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	if !tt.HasText(`Saved: ~\Desktop\Fabo report.txt`) {
		t.Fatal("report path not shown")
	}
	if err := tt.Click("Open logs folder"); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	got = strings.Join(f.actions, "\n")
	f.mu.Unlock()
	if !strings.Contains(got, `/diag/report {"containers":false}`) || !strings.Contains(got, "/diag/folder") {
		t.Fatalf("actions = %s", got)
	}

	// WSL's default memory is half of what Windows reports, not a whole
	// GB: shown and saved as the nearest GB, and not a change by itself.
	f.mu.Lock()
	f.resources = vmResources{CPUs: 8, Memory: 8151, RunningCPUs: 8, RunningMemory: 8151, MaxCPUs: 8, MaxMemory: 16303}
	f.actions = nil
	f.mu.Unlock()
	s.settingsTab = indexSettings("resources")
	s.loadSettings()
	tt.Frame()
	if !tt.HasText("8 GB") || s.memory != 8 {
		t.Fatalf("memory shown as %v", s.memory)
	}
	if err := tt.Click("Save"); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	if len(f.actions) != 0 {
		t.Fatalf("an untouched pane saved: %v", f.actions)
	}
	f.mu.Unlock()
	s.cpus = 4
	tt.Frame()
	tt.Click("Save")
	f.mu.Lock()
	got = strings.Join(f.actions, "\n")
	f.mu.Unlock()
	if !strings.Contains(got, `/resources {"cpus":4,"memory_mib":8192}`) {
		t.Fatalf("saved %s", got)
	}

	// Without fabo-win (macOS, Linux) there is no Diagnostics section.
	s.host = nil
	s.navigate(settingsPage)
	tt.Frame()
	if _, found := tt.Find("Diagnostics"); found {
		t.Fatal("Diagnostics offered without fabo-win")
	}
}
