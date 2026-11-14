//go:build linux || darwin

package localengine

import (
	"errors"
	"os"
	"path"
	"path/filepath"
	"testing"

	"github.com/OpenFabrica/Fabo/engine/internal/guestwire"
)

// Missing legacy sources may be created inside the share. A symlink into
// a sibling directory must not let legacy creation cross that boundary.
func TestSharedBindCreationStaysInsideRoot(t *testing.T) {
	parent, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(parent, "share")
	outside := filepath.Join(parent, "share-other")
	for _, dir := range []string{root, outside} {
		if err := os.Mkdir(dir, 0755); err != nil {
			t.Fatal(err)
		}
	}
	source := filepath.Join(root, "new folder", "child")
	roots := ShareRoots{root}
	if _, err := roots.source(source, false); !errors.Is(err, errInvalid) {
		t.Fatalf("structured bind unexpectedly accepted a missing source: %v", err)
	}
	got, err := roots.source(source, true)
	if want := path.Join(guestwire.HostSharePath, "new folder", "child"); err != nil || got != want {
		t.Fatalf("legacy bind mapped to %q: %v", got, err)
	}
	link := filepath.Join(root, "outside")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	if _, err := roots.source(filepath.Join(link, "child"), true); !errors.Is(err, errInvalid) {
		t.Fatalf("legacy bind accepted an escaping source: %v", err)
	}
	if _, err := os.Stat(filepath.Join(outside, "child")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("bind created data outside the share: %v", err)
	}
	roots = append(roots, outside)
	if got, err := roots.source(filepath.Join(link, "child"), true); err != nil || got != path.Join(guestwire.HostSharePath, "1", "child") {
		t.Fatalf("explicitly shared symlink target mapped to %q: %v", got, err)
	}
}
