package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/OpenFabrica/Fabo/engine/internal/buildwith"
)

// scratchBuilds is builds on a scratch PC with Fabo's builder made.
func scratchBuilds(t *testing.T, other string) (*builds, buildwith.Places) {
	t.Helper()
	dir := t.TempDir()
	p, err := buildwith.WindowsPlaces(func(k string) string {
		if k == "USERPROFILE" {
			return dir
		}
		return ""
	}, filepath.Join(dir, "Fabo"))
	if err != nil {
		t.Fatal(err)
	}
	instance := p.Instance(p.Builder)
	if err := os.MkdirAll(filepath.Dir(instance), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(instance, []byte(`{"Name":"fabo-build","Nodes":[{"Name":"fabo-build0","Endpoint":"fabo"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	b := &builds{
		places: func() (buildwith.Places, error) { return p, nil },
		other:  func() string { return other },
		logf:   func(string, ...any) {},
	}
	return b, p
}

// TestBuildsQuitAndStartAgain: quitting hands docker build back, the
// choice stays, and the next start takes it again.
func TestBuildsQuitAndStartAgain(t *testing.T) {
	b, p := scratchBuilds(t, "")
	config := `{"credsStore":"wincred","aliases":{"builder":"mine"}}`
	if err := os.WriteFile(p.Config(), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	b.apply()
	if !p.Taken() || b.status().With != buildwith.Fabo {
		t.Fatalf("a PC with no other engine builds on Fabo: %+v", b.status())
	}
	b.giveBack("quit", true)
	if p.Taken() || p.Saved() != buildwith.Fabo {
		t.Fatal("quitting must hand back and keep the choice")
	}
	if raw, _ := os.ReadFile(p.Config()); !strings.Contains(string(raw), `"mine"`) || strings.Contains(string(raw), "buildx") {
		t.Fatalf("config = %s", raw)
	}
	// A late apply in the quitting process does nothing.
	b.apply()
	if p.Taken() {
		t.Fatal("taken again behind the quit")
	}
	// The next start takes them again.
	b = &builds{places: b.places, other: b.other, logf: b.logf}
	b.apply()
	if !p.Taken() {
		t.Fatal("not taken again after starting")
	}
	if code := uninstallBuilds(b.places, b.logf); code != 0 || p.Taken() {
		t.Fatalf("uninstall = %d", code)
	}
	if _, err := os.Stat(p.State); !os.IsNotExist(err) {
		t.Fatal("build.json left behind")
	}
}

// TestBuildsBesideAnotherEngine: nothing is taken unasked, the window is
// asked, and its choice takes effect.
func TestBuildsBesideAnotherEngine(t *testing.T) {
	b, p := scratchBuilds(t, "Docker Desktop")
	b.apply()
	st := b.status()
	if p.Taken() || !st.Ask || st.With != buildwith.Docker || st.Engine != "Docker Desktop" || !st.Ready {
		t.Fatalf("status = %+v", st)
	}
	if err := b.choose(buildwith.Fabo); err != nil || !p.Taken() || b.status().Ask {
		t.Fatalf("choose = %v, %+v", err, b.status())
	}
	if err := b.choose("nope"); err == nil {
		t.Fatal("accepted an unknown choice")
	}
}

// TestBuildsQuitKeepsTheUsersOwnBuilder: a builder the user picked while
// Fabo ran survives quitting and the next start.
func TestBuildsQuitKeepsTheUsersOwnBuilder(t *testing.T) {
	b, p := scratchBuilds(t, "")
	if err := b.choose(buildwith.Fabo); err != nil || !p.Taken() {
		t.Fatalf("choose = %v", err)
	}
	theirs := `{"Key":"default","Name":"mine","Global":false}`
	if err := os.WriteFile(p.Current(), []byte(theirs), 0o600); err != nil {
		t.Fatal(err)
	}
	b.giveBack("quit", true)
	b = &builds{places: b.places, other: b.other, logf: b.logf}
	b.apply()
	if raw, _ := os.ReadFile(p.Current()); string(raw) != theirs {
		t.Fatalf("the next start took the builds back over the user's builder: %s", raw)
	}
	// A program that is gone gives back regardless.
	b.giveBack("program gone", false)
	if raw, _ := os.ReadFile(p.Config()); strings.Contains(string(raw), "buildx") {
		t.Fatalf("alias left: %s", raw)
	}
}

// TestBuildsWithoutAHome: without USERPROFILE every action fails cleanly.
func TestBuildsWithoutAHome(t *testing.T) {
	b := &builds{places: func() (buildwith.Places, error) { return buildwith.Places{}, errors.New("USERPROFILE is not set") }, other: func() string { return "" }, logf: func(string, ...any) {}}
	if st := b.status(); st.Error == "" {
		t.Fatal("no error in the status")
	}
	if b.choose(buildwith.Fabo) == nil || uninstallBuilds(b.places, b.logf) != 1 {
		t.Fatal("must fail")
	}
}

// A program moved away is noticed once; one swapped back in time is not.
func TestWatchProgram(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "app", "fabo-win.exe")
	if err := os.MkdirAll(filepath.Dir(exe), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(exe, []byte("program"), 0o755); err != nil {
		t.Fatal(err)
	}
	gone := make(chan struct{}, 2)
	watchProgram(exe, 20*time.Millisecond, func() { gone <- struct{}{} })
	select {
	case <-gone:
		t.Fatal("reported while there")
	case <-time.After(150 * time.Millisecond):
	}
	if err := os.Rename(filepath.Dir(exe), filepath.Join(dir, "moved")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-gone:
	case <-time.After(5 * time.Second):
		t.Fatal("not noticed")
	}

	swap := filepath.Join(dir, "swap.exe")
	if err := os.WriteFile(swap, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	swapped := make(chan struct{}, 1)
	watchProgram(swap, 400*time.Millisecond, func() { swapped <- struct{}{} })
	// Gone for the first look (at 400 ms), back before the second (800 ms).
	if err := os.Remove(swap); err != nil {
		t.Fatal(err)
	}
	time.Sleep(600 * time.Millisecond)
	if err := os.WriteFile(swap, []byte("new"), 0o755); err != nil {
		t.Fatal(err)
	}
	select {
	case <-swapped:
		t.Fatal("a brief swap was taken for gone")
	case <-time.After(1500 * time.Millisecond):
	}
}
