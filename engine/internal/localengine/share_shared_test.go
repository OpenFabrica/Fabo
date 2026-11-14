//go:build linux || darwin

package localengine

import (
	"os"
	"path/filepath"
	"testing"
)

// A host path that is gone still counts as shared when its nearest
// existing folder, links resolved, lies inside a share; one outside does not.
func TestMissingBindSourceInsideShares(t *testing.T) {
	share := t.TempDir()
	outside := t.TempDir()
	link := filepath.Join(t.TempDir(), "tmp")
	if err := os.Symlink(share, link); err != nil {
		t.Fatal(err)
	}
	roots, err := CanonicalShareRoots([]string{share})
	if err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]bool{
		filepath.Join(share, "gone.conf"):         true,
		filepath.Join(link, "gone", "nginx.conf"): true,
		filepath.Join(outside, "gone.conf"):       false,
	} {
		if err := roots.shared(path); (err == nil) != want {
			t.Errorf("shared(%q) = %v, want inside %v", path, err, want)
		}
	}
}
