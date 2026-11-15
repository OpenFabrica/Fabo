package main

import (
	"os"
	"path/filepath"
	"testing"
)

// The installed launcher is the one the standard LaunchAgent runs; a missing
// program or agent finds nothing outside /Applications.
func TestInstalledLauncherFollowsTheAgent(t *testing.T) {
	home := t.TempDir()
	standard := namesFor(appID)
	app := filepath.Join(home, "Apps & Co", "Fabo.app", "Contents", "MacOS")
	if err := os.MkdirAll(app, 0o755); err != nil {
		t.Fatal(err)
	}
	launcher := filepath.Join(app, "fabo")
	if err := os.WriteFile(launcher, nil, 0o755); err != nil {
		t.Fatal(err)
	}
	s := &setup{in: installation{dir: app, home: home, names: standard}}
	if err := os.MkdirAll(filepath.Dir(s.agentPath()), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.agentPath(), []byte(s.agentPlist()), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := installedLauncher(home, standard); got != launcher {
		t.Fatalf("installedLauncher = %q, want %q", got, launcher)
	}
	if err := os.Remove(launcher); err != nil {
		t.Fatal(err)
	}
	if got := installedLauncher(home, standard); got != "" && got != "/Applications/Fabo.app/Contents/MacOS/fabo" {
		t.Fatalf("a missing program gave %q", got)
	}
}
