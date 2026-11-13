//go:build linux || darwin

package localengine

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/libracoder/kiri/engine/internal/hostagent"
)

func TestImageRemovalPreservesReferencedLayersAcrossRestore(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	state, err := OpenState(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	blobs := filepath.Join(dir, "blobs")
	if err := os.Mkdir(blobs, 0700); err != nil {
		t.Fatal(err)
	}
	newLayer := func(ch string) hostagent.Layer {
		t.Helper()
		digest := strings.Repeat(ch, 64)
		path := filepath.Join(blobs, digest)
		if err := os.WriteFile(path, []byte(ch), 0600); err != nil {
			t.Fatal(err)
		}
		return hostagent.Layer{Digest: "sha256:" + digest, Path: path}
	}
	shared, inUse, unused := newLayer("a"), newLayer("b"), newLayer("c")
	seeded := identifyImage(Image{Layers: []hostagent.Layer{shared, inUse}})
	pulled := identifyImage(Image{Layers: []hostagent.Layer{shared, unused}})
	catalog := map[string]Image{"seed:local": seeded}
	if err := saveJSON(dir, "images.json", map[string]Image{"pulled:local": pulled}); err != nil {
		t.Fatal(err)
	}
	index, err := state.restoreImages()
	if err != nil {
		t.Fatal(err)
	}
	e := &Engine{state: state, catalog: catalog, imageIndex: index, images: index.references(catalog),
		containers: map[string]*container{"kept": {name: "kept", imageID: seeded.ID, blobs: seeded.Layers}}}
	if err := e.putImage("seed:second", seeded); err != nil {
		t.Fatal(err)
	}
	if _, err := e.removeImage("seed:local", false); err != nil {
		t.Fatal(err)
	}
	if _, err := e.removeImage("seed:second", false); !errors.Is(err, errConflict) {
		t.Fatalf("last referenced tag removal: %v", err)
	}
	if _, err := e.removeImage("seed:second", true); err != nil {
		t.Fatal(err)
	}
	reloaded, err := state.restoreImages()
	if err != nil {
		t.Fatal(err)
	}
	refs := reloaded.references(catalog)
	if len(refs) != 1 || refs["pulled:local"].ID != pulled.ID {
		t.Fatalf("restored references: %+v", refs)
	}
	if _, err := e.removeImage("pulled:local", false); err != nil {
		t.Fatal(err)
	}
	if reclaimed, err := e.collectUnusedLayers(); err != nil || reclaimed != 1 {
		t.Fatalf("reclaimed = %d, %v", reclaimed, err)
	}
	for _, layer := range []hostagent.Layer{shared, inUse} {
		if _, err := os.Stat(layer.Path); err != nil {
			t.Fatalf("container's layer was reclaimed: %v", err)
		}
	}
	delete(e.containers, "kept")
	if reclaimed, err := e.collectUnusedLayers(); err != nil || reclaimed != 2 {
		t.Fatalf("reclaimed after container removal = %d, %v", reclaimed, err)
	}
}
