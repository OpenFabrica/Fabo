package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/OpenFabrica/Fabo/engine/internal/winhost"
)

// fakeSystem is a PC for setup to act on, recording what it was asked.
type fakeSystem struct {
	mu        sync.Mutex
	wsl       bool
	list      []string
	boot      time.Time
	alive     bool
	wslCode   uint32
	testOut   string
	toolsErr  error
	calls     []string
	autostart *bool
	dir       string
}

func (f *fakeSystem) record(s string) {
	f.mu.Lock()
	f.calls = append(f.calls, s)
	f.mu.Unlock()
}
func (f *fakeSystem) arch() string       { return "amd64" }
func (f *fakeSystem) wslInstalled() bool { f.mu.Lock(); defer f.mu.Unlock(); return f.wsl }
func (f *fakeSystem) distros() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string{}, f.list...)
}
func (f *fakeSystem) bootTime() time.Time   { return f.boot }
func (f *fakeSystem) engineAlive() bool     { f.mu.Lock(); defer f.mu.Unlock(); return f.alive }
func (f *fakeSystem) otherDocker() bool     { return false }
func (f *fakeSystem) restartWindows() error { f.record("restart"); return nil }
func (f *fakeSystem) installWSL(msi, sum string) (uint32, error) {
	f.record("installWSL " + filepath.Base(msi))
	f.mu.Lock()
	if f.wslCode == 0 {
		f.wsl = true
	}
	f.mu.Unlock()
	return f.wslCode, nil
}
func (f *fakeSystem) resumeAfterRestart() error { f.record("resume"); return nil }
func (f *fakeSystem) importDistro(name, dir, tarball string) error {
	f.record("import " + name)
	f.mu.Lock()
	f.list = append(f.list, name)
	f.mu.Unlock()
	return nil
}
func (f *fakeSystem) installUbuntu(file, dir string) error { f.record("ubuntu"); return nil }
func (f *fakeSystem) installEngine(distro string) error    { f.record("engine " + distro); return nil }
func (f *fakeSystem) startFaboLinux() error {
	f.record("start")
	f.mu.Lock()
	f.alive = true
	f.mu.Unlock()
	return nil
}
func (f *fakeSystem) setDistro(d string)  { f.record("distro " + d) }
func (f *fakeSystem) ensureEngine() error {
	f.record("ensure")
	f.mu.Lock()
	f.alive = true
	f.mu.Unlock()
	return nil
}
func (f *fakeSystem) testContainer() (string, error) {
	if strings.Contains(f.testOut, "hello") {
		return f.testOut, nil
	}
	return f.testOut, errors.New("exit status 125")
}
func (f *fakeSystem) runTools(makeDefault bool, done func(int)) error {
	f.record("tools")
	if f.toolsErr != nil {
		return f.toolsErr
	}
	for i := range 5 {
		done(i)
	}
	return nil
}
func (f *fakeSystem) setAutostart(on bool) error { f.autostart = &on; return nil }
func (f *fakeSystem) verified(rel string) (string, error) {
	p := filepath.Join(f.dir, rel)
	os.WriteFile(p, []byte("tar"), 0o644)
	return p, nil
}
func (f *fakeSystem) logf(string, ...any) {}

// waitIdle waits for the running step to end.
func waitIdle(t *testing.T, s *setup) setupView {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for s.isBusy() {
		if time.Now().After(deadline) {
			t.Fatal("setup step did not finish")
		}
		time.Sleep(5 * time.Millisecond)
	}
	return s.state()
}

// TestSetupFaboLinux walks a PC that has WSL through Fabo Linux to done.
func TestSetupFaboLinux(t *testing.T) {
	dir := t.TempDir()
	sys := &fakeSystem{wsl: true, testOut: "Unable to find image 'alpine'\nStatus: Downloaded\nhello\n", dir: dir}
	s := newSetup(sys, dir)
	if v := s.state(); v.Step != "choose" || !v.MakeDefault || !v.Autostart {
		t.Fatalf("start = %+v", v)
	}
	if err := s.action("choose", json.RawMessage(`"fabo"`)); err != nil {
		t.Fatal(err)
	}
	v := waitIdle(t, s)
	if v.Step != "ready" || v.Error != "" {
		t.Fatalf("after choose: step %q, error %q", v.Step, v.Error)
	}
	saved := winhost.LoadSaved(dir)
	if saved.Distro != winhost.Distro || saved.Linux != "fabo" || saved.Done {
		t.Fatalf("saved = %+v", saved)
	}
	want := "import fabo,start,distro fabo,ensure,tools"
	if got := strings.Join(sys.calls, ","); got != want {
		t.Fatalf("calls %s, want %s", got, want)
	}
	if !needed(sys, dir) {
		t.Fatal("setup not needed before finishing")
	}
	s.action("autostart", json.RawMessage(`false`))
	finished := false
	s.finished = func() { finished = true }
	if err := s.action("finish", nil); err != nil {
		t.Fatal(err)
	}
	if !finished || sys.autostart == nil || *sys.autostart || needed(sys, dir) || s.state().Step != "done" {
		t.Fatalf("finish: finished %v autostart %v needed %v", finished, sys.autostart, needed(sys, dir))
	}
	// The distro going away brings setup back.
	sys.list = nil
	if !needed(sys, dir) {
		t.Fatal("setup not needed with its distro gone")
	}
}

// TestSetupTestContainerFailure shows the engine's own words.
func TestSetupTestContainerFailure(t *testing.T) {
	dir := t.TempDir()
	sys := &fakeSystem{wsl: true, list: []string{"Ubuntu-24.04"}, testOut: "Unable to find image 'alpine:latest' locally\ndocker: Error response from daemon: Get \"https://registry-1.docker.io/v2/\": dial tcp: i/o timeout.\nRun 'docker run --help' for more information\n", dir: dir}
	s := newSetup(sys, dir)
	if v := s.state(); v.ExistingUbuntu != "Ubuntu-24.04" {
		t.Fatalf("existing ubuntu = %q", v.ExistingUbuntu)
	}
	s.action("choose", json.RawMessage(`"ubuntu"`))
	v := waitIdle(t, s)
	if v.Step != "engine" || !strings.HasPrefix(v.Error, "couldn't pull alpine: Get ") {
		t.Fatalf("step %q error %q", v.Step, v.Error)
	}
	if v.Tasks[2].State != "error" {
		t.Fatalf("tasks = %+v", v.Tasks)
	}
	if got := winhost.LoadSaved(dir); got.Distro != "Ubuntu-24.04" || got.LastError == "" {
		t.Fatalf("saved = %+v", got)
	}
	// Setup resumed later, with the engine answering, carries on at the
	// tools, showing why it stopped until it is retried.
	s2 := newSetup(sys, dir)
	if v := s2.state(); v.Step != "tools" || v.Error == "" {
		t.Fatalf("resumed = %q %q", v.Step, v.Error)
	}
	s2.action("retry", nil)
	if v := waitIdle(t, s2); v.Step != "ready" {
		t.Fatalf("retry = %q %q", v.Step, v.Error)
	}
}

// TestSetupInstallsWSL downloads the pinned MSI, checks it, and asks for
// a restart when the installer says so.
func TestSetupInstallsWSL(t *testing.T) {
	body := []byte("not really an msi")
	sum := sha256.Sum256(body)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(body) }))
	defer srv.Close()
	saved := wslMSI["amd64"]
	t.Cleanup(func() { wslMSI["amd64"] = saved })
	wslMSI["amd64"] = pin{"WSL test", srv.URL + "/wsl.msi", hex.EncodeToString(sum[:]), int64(len(body)), "wsl.msi"}

	dir := t.TempDir()
	sys := &fakeSystem{wslCode: 3010, boot: time.Now().Add(-time.Hour), dir: dir}
	s := newSetup(sys, dir)
	if s.state().Step != "welcome" {
		t.Fatalf("step %q", s.state().Step)
	}
	s.action("install-wsl", nil)
	v := waitIdle(t, s)
	if v.Step != "restart" || v.Error != "" {
		t.Fatalf("step %q error %q", v.Step, v.Error)
	}
	if got := strings.Join(sys.calls, ","); got != "installWSL wsl.msi,resume" {
		t.Fatalf("calls %s", got)
	}
	// Still before the restart: the restart step again. After it: choose.
	sys.wsl = true
	if st := newSetup(sys, dir).state().Step; st != "restart" {
		t.Fatalf("before restart: %q", st)
	}
	sys.boot = time.Now().Add(time.Second)
	if st := newSetup(sys, dir).state().Step; st != "choose" {
		t.Fatalf("after restart: %q", st)
	}

	// A file that does not match its hash is refused and not kept.
	wslMSI["amd64"] = pin{"WSL test", srv.URL + "/wsl.msi", strings.Repeat("0", 64), int64(len(body)), "bad.msi"}
	os.Remove(filepath.Join(dir, "downloads", "wsl.msi"))
	sys2 := &fakeSystem{dir: dir}
	s3 := newSetup(sys2, dir)
	s3.action("install-wsl", nil)
	if v := waitIdle(t, s3); !strings.Contains(v.Error, "did not match its SHA-256") {
		t.Fatalf("bad hash: %q", v.Error)
	}
	if _, err := os.Stat(filepath.Join(dir, "downloads", "bad.msi")); err == nil {
		t.Fatal("mismatched download kept")
	}
	if len(sys2.calls) != 0 {
		t.Fatalf("installed after a bad download: %v", sys2.calls)
	}
}

// TestSetupBusy refuses a second step while one runs.
func TestSetupBusy(t *testing.T) {
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "100")
		w.Write([]byte("x"))
		w.(http.Flusher).Flush()
		select {
		case <-block:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(block)
	saved := wslMSI["amd64"]
	t.Cleanup(func() { wslMSI["amd64"] = saved })
	wslMSI["amd64"] = pin{"WSL test", srv.URL, strings.Repeat("0", 64), 100, "slow.msi"}
	dir := t.TempDir()
	s := newSetup(&fakeSystem{dir: dir}, dir)
	if err := s.action("install-wsl", nil); err != nil {
		t.Fatal(err)
	}
	if err := s.action("install-wsl", nil); !errors.Is(err, errBusy) {
		t.Fatalf("second step: %v", err)
	}
	// Cancel returns to the step's start without an error.
	s.action("cancel", nil)
	if v := waitIdle(t, s); v.Error != "" || v.Step != "welcome" {
		t.Fatalf("after cancel: %q %q", v.Step, v.Error)
	}
}
