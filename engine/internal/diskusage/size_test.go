//go:build linux || darwin

package diskusage

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestAllocatedCountsBlocksAndHardLinks(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "sparse")
	f, err := os.Create(file)
	if err != nil {
		t.Fatal(err)
	}
	_, writeErr := f.WriteAt([]byte{1}, 16<<20)
	if err := f.Close(); err != nil || writeErr != nil {
		t.Fatalf("create sparse file: %v, %v", writeErr, err)
	}
	if err := os.Link(file, filepath.Join(root, "hardlink")); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "external")
	if err := os.Symlink(t.TempDir(), link); err != nil {
		t.Fatal(err)
	}
	var want uint64
	for _, path := range []string{root, file, link} {
		info, err := os.Lstat(path)
		if err != nil {
			t.Fatal(err)
		}
		want += uint64(info.Sys().(*syscall.Stat_t).Blocks) * 512
	}
	got, err := Allocated(t.Context(), root)
	if err != nil || got != want || got >= 16<<20 {
		t.Fatalf("allocated %d, want %d: %v", got, want, err)
	}
}
