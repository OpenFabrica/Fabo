//go:build linux || darwin

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNamesFollowTheBundleIdentifier(t *testing.T) {
	plist := "<dict><key>CFBundleName</key><string>Kiri Go</string>\n<key>CFBundleIdentifier</key><string> dev.kiri.app.go </string></dict>"
	if id := bundleID(plist); id != "dev.kiri.app.go" {
		t.Fatalf("bundleID = %q", id)
	}
	standard := names{app: "Kiri", state: ".kiri", context: "kiri", builder: "kiri-build", agent: "dev.kiri.daemon", id: "dev.kiri.app"}
	for id, want := range map[string]names{
		"":                    standard,
		"dev.kiri.app":        standard,
		"dev.kiri.app.":       standard,
		"dev.kiri.app.My-App": standard,
		"dev.kiri.app.dev":    {app: "Kiri Dev", state: ".kiri-dev", context: "kiri-dev", builder: "kiri-dev-build", agent: "dev.kiri.daemon.dev", id: "dev.kiri.app.dev", flavored: true},
		"dev.kiri.app.go":     {app: "Kiri Go", state: ".kiri-go", context: "kiri-go", builder: "kiri-go-build", agent: "dev.kiri.daemon.go", id: "dev.kiri.app.go", flavored: true},
	} {
		if got := namesFor(id); got != want {
			t.Errorf("namesFor(%q) = %+v, want %+v", id, got, want)
		}
	}
}

func TestCheckStoreProtectsAnUnimportedRustStore(t *testing.T) {
	state, legacy := filepath.Join(t.TempDir(), "go-engine"), t.TempDir()
	t.Setenv("KIRI_ENGINE", "")
	if err := checkStore(state, filepath.Join(legacy, "missing")); err != nil {
		t.Fatalf("missing store: %v", err)
	}
	if err := checkStore(state, legacy); err != nil {
		t.Fatalf("empty store: %v", err)
	}
	if err := os.WriteFile(filepath.Join(legacy, "index.json"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := checkStore(state, legacy); err == nil || !strings.Contains(err.Error(), "migrate-rust") {
		t.Fatalf("populated store: %v", err)
	}
	// An import, or a Go disk that already exists, settles the choice.
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state, "rust-import.json"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := checkStore(state, legacy); err != nil {
		t.Fatalf("imported store: %v", err)
	}
	if err := os.Remove(filepath.Join(state, "rust-import.json")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KIRI_ENGINE", "go")
	if err := checkStore(state, legacy); err != nil {
		t.Fatalf("KIRI_ENGINE=go: %v", err)
	}
	t.Setenv("KIRI_ENGINE", "rust")
	if err := checkStore(state, legacy); err == nil {
		t.Fatal("KIRI_ENGINE=rust must fail: there is no Rust engine")
	}
}
