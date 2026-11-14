//go:build linux || darwin

package localengine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/OpenFabrica/Fabo/engine/internal/hostagent"
)

func TestDiskUsageFindsBlobAfterStatePathChanges(t *testing.T) {
	dir := t.TempDir()
	digest := strings.Repeat("a", 64)
	blobs := filepath.Join(dir, "blobs")
	if err := os.Mkdir(blobs, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(blobs, digest), []byte("payload"), 0600); err != nil {
		t.Fatal(err)
	}
	image := Image{ID: "sha256:" + strings.Repeat("b", 64), Layers: []hostagent.Layer{
		{Digest: "sha256:" + digest, Path: filepath.Join(dir, "old", digest)},
	}}
	e := &Engine{state: &State{dir: dir}, images: map[string]Image{"test:latest": image}}
	report, err := e.measureDiskUsage(t.Context())
	if err != nil || report.LayersSize != 7 {
		t.Fatalf("layer size after path change: %d, %v", report.LayersSize, err)
	}
}
