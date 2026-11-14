//go:build linux || darwin

package main

import (
	"archive/tar"
	"bytes"
	"io"
	"slices"
	"strings"
	"testing"
)

// A session shares the target's PID and network namespaces, keeps the
// installed packages in the shared volume and is removed when it ends; it
// asks for a terminal only when there is one.
func TestDebugRunArgs(t *testing.T) {
	id := "0123456789abcdef0123456789abcdef"
	args := debugRunArgs(id, "shop-web-1", "/app", true)
	joined := strings.Join(args, " ")
	for _, want := range []string{"run --rm -it", "--pid container:" + id, "--network container:" + id, "--cap-add SYS_PTRACE",
		"-v fabo-debug:/var/cache/fabo-debug", "FABO_DEBUG_TARGET=shop-web-1", "FABO_DEBUG_WORKDIR=/app", "--label dev.fabo.debug=shop-web-1"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing %q in %s", want, joined)
		}
	}
	if args[len(args)-1] != debugToolbox || args[len(args)-3] != "--entrypoint" {
		t.Fatalf("toolbox: %q", args)
	}
	if piped := debugRunArgs(id, "x", "", false); slices.Contains(piped, "-it") || !slices.Contains(piped, "-i") {
		t.Fatalf("without a terminal: %q", piped)
	}
}

// The toolbox's scripts land executable at their paths.
func TestDebugArchive(t *testing.T) {
	raw, err := debugArchive()
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(bytes.NewReader(raw))
	seen := map[string]int64{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(tr)
		if string(body) != debugFiles[h.Name] {
			t.Fatalf("%s content differs", h.Name)
		}
		seen[h.Name] = h.Mode
	}
	if len(seen) != len(debugFiles) || seen["usr/local/bin/dctl"] != 0o755 || seen["usr/local/bin/fabo-debug-shell"] != 0o755 || seen["etc/fish/conf.d/fabo-debug.fish"] != 0o644 {
		t.Fatalf("entries: %v", seen)
	}
}
