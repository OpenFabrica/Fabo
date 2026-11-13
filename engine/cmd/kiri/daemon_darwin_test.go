package main

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestDaemonCommandUsesTheBundleAndPrivateDirectories(t *testing.T) {
	home := t.TempDir()
	bundle := filepath.Join(t.TempDir(), "Kiri Dev.app", "Contents")
	macos, guest := filepath.Join(bundle, "MacOS"), filepath.Join(bundle, "Resources", "guest")
	for _, dir := range []string{macos, guest} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, file := range []string{filepath.Join(macos, "kiri-go-daemon"), filepath.Join(guest, "Image"), filepath.Join(guest, "bzImage")} {
		if err := os.WriteFile(file, nil, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// A run directory left by the Rust daemon, open to other users.
	run := filepath.Join(home, ".kiri-dev", "run")
	if err := os.MkdirAll(run, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KIRI_SOCKET", "")
	t.Setenv("KIRI_ENGINE", "")
	t.Setenv("KIRI_DATA_DIR", "")
	t.Setenv("KIRI_CPUS", "1")
	t.Setenv("KIRI_MEMORY_MIB", "3072")

	in := installation{dir: macos, home: home, names: namesFor("dev.kiri.app.dev")}
	engine, args, err := in.daemonCommand()
	if err != nil {
		t.Fatal(err)
	}
	if engine != filepath.Join(macos, "kiri-go-daemon") {
		t.Fatalf("engine = %s", engine)
	}
	state := filepath.Join(home, ".kiri-dev", "go-engine")
	for _, dir := range []string{run, state} {
		if info, err := os.Stat(dir); err != nil || info.Mode().Perm() != 0o700 {
			t.Fatalf("%s: %v, want a private directory", dir, info)
		}
	}
	value := func(flag string) string {
		if i := slices.Index(args, flag); i >= 0 && i+1 < len(args) {
			return args[i+1]
		}
		return ""
	}
	for flag, want := range map[string]string{
		"-socket":            filepath.Join(run, "docker.sock"),
		"-state":             state,
		"-finder-mount":      filepath.Join(home, "Kiri Dev"),
		"-disk":              filepath.Join(state, "data.img"),
		"-cpus":              "1",
		"-memory":            "3072",
		"-builder-container": "buildx_buildkit_kiri-dev-build0",
	} {
		if got := value(flag); got != want {
			t.Errorf("%s = %q, want %q", flag, got, want)
		}
	}
	// The machine-wide domain listeners belong to the installed app.
	if slices.Contains(args, "-domains-default") {
		t.Error("a flavored bundle must not claim the default domain ports")
	}
}

func TestDefaultCPUsStayWithinTheMac(t *testing.T) {
	for env, want := range map[string]int{"": 2, "junk": 2, "0": 1, "3": 3, "64": 4} {
		t.Setenv("KIRI_CPUS", env)
		if got := defaultCPUs(4); got != want {
			t.Errorf("KIRI_CPUS=%q: %d, want %d", env, got, want)
		}
	}
}
