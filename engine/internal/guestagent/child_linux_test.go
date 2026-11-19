package guestagent

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// A missing working directory and its missing parents are made 0755 even
// under a restrictive umask, so a non-root user can enter them; existing
// directories keep their mode.
func TestMakeWorkdirCreatesParentsWithMode0755(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "made"), 0700); err != nil {
		t.Fatal(err)
	}
	old := syscall.Umask(0o077)
	defer syscall.Umask(old)
	makeWorkdir(root, "/made/for/1000/")
	for name, mode := range map[string]os.FileMode{"made": 0700, "made/for": 0755, "made/for/1000": 0755} {
		info, err := os.Stat(filepath.Join(root, name))
		if err != nil || !info.IsDir() || info.Mode().Perm() != mode {
			t.Errorf("%s: %v, %v, want a directory with mode %v", name, info, err, mode)
		}
	}
	// An empty working directory means the root; a file in the way is left
	// for the later chdir to report.
	makeWorkdir(root, "")
	if err := os.WriteFile(filepath.Join(root, "file"), nil, 0644); err != nil {
		t.Fatal(err)
	}
	makeWorkdir(root, "/file/below")
	if info, err := os.Stat(filepath.Join(root, "file")); err != nil || !info.Mode().IsRegular() {
		t.Errorf("file in the way was changed: %v, %v", info, err)
	}
}
