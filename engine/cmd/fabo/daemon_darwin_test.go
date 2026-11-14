package main

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestDaemonCommandUsesTheBundleAndPrivateDirectories(t *testing.T) {
	home := t.TempDir()
	bundle := filepath.Join(t.TempDir(), "Fabo Dev.app", "Contents")
	macos, guest := filepath.Join(bundle, "MacOS"), filepath.Join(bundle, "Resources", "guest")
	for _, dir := range []string{macos, guest} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, file := range []string{filepath.Join(macos, "fabo-daemon"), filepath.Join(guest, "Image"), filepath.Join(guest, "bzImage")} {
		if err := os.WriteFile(file, nil, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// A run directory left by the Rust daemon, open to other users.
	run := filepath.Join(home, ".fabo-dev", "run")
	if err := os.MkdirAll(run, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FABO_SOCKET", "")
	t.Setenv("FABO_ENGINE", "")
	t.Setenv("FABO_DATA_DIR", "")
	t.Setenv("FABO_CPUS", "1")
	t.Setenv("FABO_MEMORY_MIB", "3072")

	in := installation{dir: macos, home: home, names: namesFor("dev.fabo.app.dev")}
	engine, args, err := in.daemonCommand()
	if err != nil {
		t.Fatal(err)
	}
	if engine != filepath.Join(macos, "fabo-daemon") {
		t.Fatalf("engine = %s", engine)
	}
	state := filepath.Join(home, ".fabo-dev", "go-engine")
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
		"-finder-mount":      filepath.Join(home, "Fabo Dev"),
		"-disk":              filepath.Join(state, "data.img"),
		"-cpus":              "1",
		"-memory":            "3072",
		"-builder-container": "buildx_buildkit_fabo-dev-build0",
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
		t.Setenv("FABO_CPUS", env)
		if got := defaultCPUs(4); got != want {
			t.Errorf("FABO_CPUS=%q: %d, want %d", env, got, want)
		}
	}
}
