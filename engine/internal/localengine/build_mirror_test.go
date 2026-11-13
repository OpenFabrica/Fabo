//go:build linux || darwin

package localengine

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/types"
	"github.com/libracoder/kiri/engine/internal/hostagent"
)

func TestBuildMirrorServesExactImageAndScopesBlobs(t *testing.T) {
	digest := func(raw []byte) string { return fmt.Sprintf("sha256:%x", sha256.Sum256(raw)) }
	config := []byte(`{"architecture":"arm64","os":"linux"}`)
	layer := []byte("test layer bytes")
	file := filepath.Join(t.TempDir(), "layer")
	if err := os.WriteFile(file, layer, 0600); err != nil {
		t.Fatal(err)
	}
	image := Image{ID: digest(config), ConfigBlob: config, Layers: []hostagent.Layer{
		{Digest: digest(layer), MediaType: string(types.OCIUncompressedLayer), Path: file},
	}}
	e := &Engine{images: map[string]Image{"index.docker.io/library/mirror-fixture:dev": image}}
	m := &BuildMirror{token: "fixture-token", engine: func() *Engine { return e }}
	const route = "/v2/fixture-token/docker.io/library/mirror-fixture/"
	request := func(method, path, bytesRange string, want int) *httptest.ResponseRecorder {
		t.Helper()
		w := httptest.NewRecorder()
		r := httptest.NewRequest(method, path, nil)
		if bytesRange != "" {
			r.Header.Set("Range", bytesRange)
		}
		m.ServeHTTP(w, r)
		if w.Code != want {
			t.Fatalf("%s %s: %d, want %d", method, path, w.Code, want)
		}
		return w
	}
	w := request(http.MethodGet, route+"manifests/dev?ns=docker.io", "", 200)
	var manifest v1.Manifest
	if err := json.Unmarshal(w.Body.Bytes(), &manifest); err != nil || manifest.Config.Digest.String() != image.ID ||
		len(manifest.Layers) != 1 || manifest.Layers[0].Digest.String() != digest(layer) || manifest.Layers[0].Size != int64(len(layer)) {
		t.Fatalf("unexpected manifest: %s (%v)", w.Body.String(), err)
	}
	manifestDigest := digest(w.Body.Bytes())
	if w.Header().Get("Docker-Content-Digest") != manifestDigest {
		t.Fatal("manifest digest header did not match the payload")
	}
	head := request(http.MethodHead, route+"manifests/"+manifestDigest, "", 200)
	if head.Body.Len() != 0 || head.Header().Get("Docker-Content-Digest") != manifestDigest {
		t.Fatal("manifest HEAD did not match the GET response")
	}
	if got := request(http.MethodGet, route+"blobs/"+image.ID, "", 200).Body.String(); got != string(config) {
		t.Fatalf("config bytes changed: %q", got)
	}
	if got := request(http.MethodGet, route+"blobs/"+digest(layer), "bytes=5-9", 206).Body.String(); got != "layer" {
		t.Fatalf("layer range: %q", got)
	}
	request(http.MethodGet, "/v2/wrong/docker.io/library/mirror-fixture/manifests/dev", "", 404)
	request(http.MethodGet, "/v2/fixture-token/docker.io/library/other/blobs/"+digest(layer), "", 404)
	request(http.MethodGet, route+"manifests/missing", "", 404)
	request(http.MethodPut, route+"manifests/dev", "", 405)
}
