//go:build linux || darwin

package localengine

import (
	"crypto/sha256"
	"fmt"
	"os"
	"testing"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/static"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

type blobSource struct {
	v1.Image
	layer v1.Layer
}

func (s blobSource) LayerByDigest(v1.Hash) (v1.Layer, error) { return s.layer, nil }

func TestCacheBlobPublishesOnlyVerifiedContent(t *testing.T) {
	dir := t.TempDir()
	want := []byte("good")
	digest := v1.Hash{Algorithm: "sha256", Hex: fmt.Sprintf("%x", sha256.Sum256(want))}
	desc := v1.Descriptor{Digest: digest, Size: int64(len(want))}
	if _, err := cacheBlob(dir, blobSource{layer: static.NewLayer([]byte("evil"), types.OCILayer)}, desc); err == nil {
		t.Fatal("cached incorrect blob with the same size")
	}
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
		t.Fatalf("failed upload left cache entries: %v, %v", entries, err)
	}
	path, err := cacheBlob(dir, blobSource{layer: static.NewLayer(want, types.OCILayer)}, desc)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != string(want) {
		t.Fatalf("cached blob = %q, %v", got, err)
	}
	if got, err := cacheBlob(dir, blobSource{}, desc); err != nil || got != path {
		t.Fatalf("cache did not reuse verified blob: %q, %v", got, err)
	}
}
