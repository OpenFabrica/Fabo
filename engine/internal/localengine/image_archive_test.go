//go:build linux || darwin

package localengine

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestImageArchiveValidatesEveryImageBeforePublishing(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	state, err := OpenState(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = state.Close() })
	handler := (&Engine{state: state}).Handler()

	var layer bytes.Buffer
	tw := tar.NewWriter(&layer)
	writeArchiveMember(t, tw, "message", []byte("test image\n"))
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	digest := fmt.Sprintf("%x", sha256.Sum256(layer.Bytes()))
	config := fmt.Appendf(nil, "{\n  \"os\":\"linux\",\"architecture\":%q,\"rootfs\":{\"type\":\"layers\",\"diff_ids\":[\"sha256:%s\"]}\n}\n", runtime.GOARCH, digest)
	layerName := "blobs/sha256/" + digest
	configName := fmt.Sprintf("blobs/sha256/%x", sha256.Sum256(config))
	aliasName := "blobs/sha256/" + strings.Repeat("a", 64)
	makeArchive := func(secondLayer string) *bytes.Buffer {
		var buf bytes.Buffer
		tw := tar.NewWriter(&buf)
		writeArchiveMember(t, tw, layerName, layer.Bytes())
		writeArchiveMember(t, tw, configName, config)
		if err := tw.WriteHeader(&tar.Header{Name: aliasName, Typeflag: tar.TypeLink, Linkname: layerName}); err != nil {
			t.Fatal(err)
		}
		raw, err := json.Marshal([]archiveManifestEntry{
			{Config: configName, RepoTags: []string{"fixture:one"}, Layers: []string{layerName}},
			{Config: configName, RepoTags: []string{"fixture:two"}, Layers: []string{secondLayer}},
		})
		if err != nil {
			t.Fatal(err)
		}
		writeArchiveMember(t, tw, "manifest.json", raw)
		if err := tw.Close(); err != nil {
			t.Fatal(err)
		}
		return &buf
	}
	request := func(method, path string, body io.Reader) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest(method, path, body))
		return w
	}

	bad := request(http.MethodPost, "/v1.44/images/load", makeArchive(aliasName))
	if bad.Code != http.StatusBadRequest || !strings.Contains(bad.Body.String(), "digest mismatch") {
		t.Fatalf("invalid second image: %d %s", bad.Code, bad.Body)
	}
	for _, path := range []string{"image-index.json", "blobs"} {
		if _, err := os.Stat(filepath.Join(dir, path)); !os.IsNotExist(err) {
			t.Fatalf("invalid import changed %s: %v", path, err)
		}
	}
	good := request(http.MethodPost, "/v1.44/images/load", makeArchive(layerName))
	if good.Code != http.StatusOK || !strings.Contains(good.Body.String(), "Loaded image: fixture:two") {
		t.Fatalf("valid import: %d %s", good.Code, good.Body)
	}
	index, err := state.restoreImages()
	if err != nil {
		t.Fatal(err)
	}
	references := index.references(nil)
	if len(references) != 2 {
		t.Fatalf("restored %d tags, want 2", len(references))
	}
	for _, image := range references {
		if !bytes.Equal(image.ConfigBlob, config) {
			t.Fatal("restored image changed the original config bytes")
		}
	}

	saved := request(http.MethodGet, "/v1.44/images/get?names=fixture", nil)
	if saved.Code != http.StatusOK {
		t.Fatalf("export: %d %s", saved.Code, saved.Body)
	}
	tr := tar.NewReader(saved.Body)
	for {
		h, err := tr.Next()
		if err != nil {
			t.Fatalf("export missing original config %q: %v", configName, err)
		}
		if h.Name != configName {
			continue
		}
		raw, err := io.ReadAll(tr)
		if err != nil || !bytes.Equal(raw, config) {
			t.Fatalf("export changed original config: %v", err)
		}
		break
	}
}

func writeArchiveMember(t *testing.T, w *tar.Writer, name string, content []byte) {
	t.Helper()
	if err := w.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0644, Size: int64(len(content))}); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(content); err != nil {
		t.Fatal(err)
	}
}
