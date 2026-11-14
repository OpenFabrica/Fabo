//go:build linux || darwin

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNamesFollowTheBundleIdentifier(t *testing.T) {
	plist := "<dict><key>CFBundleName</key><string>Fabo Go</string>\n<key>CFBundleIdentifier</key><string> dev.fabo.app.go </string></dict>"
	if id := bundleID(plist); id != "dev.fabo.app.go" {
		t.Fatalf("bundleID = %q", id)
	}
	standard := names{app: "Fabo", state: ".fabo", context: "fabo", builder: "fabo-build", agent: "dev.fabo.daemon", id: "dev.fabo.app"}
	for id, want := range map[string]names{
		"":                    standard,
		"dev.fabo.app":        standard,
		"dev.fabo.app.":       standard,
		"dev.fabo.app.My-App": standard,
		"dev.fabo.app.dev":    {app: "Fabo Dev", state: ".fabo-dev", context: "fabo-dev", builder: "fabo-dev-build", agent: "dev.fabo.daemon.dev", id: "dev.fabo.app.dev", flavored: true},
		"dev.fabo.app.go":     {app: "Fabo Go", state: ".fabo-go", context: "fabo-go", builder: "fabo-go-build", agent: "dev.fabo.daemon.go", id: "dev.fabo.app.go", flavored: true},
	} {
		if got := namesFor(id); got != want {
			t.Errorf("namesFor(%q) = %+v, want %+v", id, got, want)
		}
	}
}

func TestCheckStoreProtectsAnUnimportedKiriStore(t *testing.T) {
	state, legacy := filepath.Join(t.TempDir(), "go-engine"), t.TempDir()
	t.Setenv("FABO_ENGINE", "")
	if err := checkStore(state, filepath.Join(legacy, "missing")); err != nil {
		t.Fatalf("missing store: %v", err)
	}
	if err := checkStore(state, legacy); err != nil {
		t.Fatalf("empty store: %v", err)
	}
	if err := os.WriteFile(filepath.Join(legacy, "index.json"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := checkStore(state, legacy); err == nil || !strings.Contains(err.Error(), "migrate") {
		t.Fatalf("populated store: %v", err)
	}
	// An import, or a Go disk that already exists, settles the choice.
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state, "kiri-import.json"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := checkStore(state, legacy); err != nil {
		t.Fatalf("imported store: %v", err)
	}
	if err := os.Remove(filepath.Join(state, "kiri-import.json")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FABO_ENGINE", "go")
	if err := checkStore(state, legacy); err != nil {
		t.Fatalf("FABO_ENGINE=go: %v", err)
	}
	t.Setenv("FABO_ENGINE", "rust")
	if err := checkStore(state, legacy); err == nil {
		t.Fatal("FABO_ENGINE=rust must fail: this build has only the Go engine")
	}
}
