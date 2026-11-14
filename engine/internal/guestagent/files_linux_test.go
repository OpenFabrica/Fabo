package guestagent

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/OpenFabrica/Fabo/engine/internal/guestwire"
)

func TestFileViewRootAndFileTypes(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "file"), []byte("abcde"), 0600); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("other"), 0600); err != nil {
		t.Fatal(err)
	}
	for name, target := range map[string]string{"link": "/file", "outside": outside} {
		if err := os.Symlink(target, filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	if err := unix.Mkfifo(filepath.Join(dir, "fifo"), 0600); err != nil {
		t.Fatal(err)
	}
	root, err := os.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	data, err := fileViewRead(root, guestwire.FileQuery{Path: "/link", Offset: 2, Count: 8})
	if err != nil || string(data) != "cde" {
		t.Fatalf("read container symlink: %q, %v", data, err)
	}
	if _, err := fileViewRead(root, guestwire.FileQuery{Path: "/outside", Count: 8}); !errors.Is(err, unix.ENOENT) {
		t.Fatalf("read link outside root: %v", err)
	}
	if _, err := fileViewRead(root, guestwire.FileQuery{Path: "/fifo", Count: 8}); !errors.Is(err, unix.EINVAL) {
		t.Fatalf("read FIFO: %v", err)
	}
	entries, err := fileViewList(root, "/")
	if err != nil || len(entries) != 4 || entries[2].Name != "link" ||
		entries[2].Mode&unix.S_IFMT != unix.S_IFREG || entries[3].Mode&unix.S_IFMT != unix.S_IFLNK {
		t.Fatalf("list container root: %+v, %v", entries, err)
	}
}
