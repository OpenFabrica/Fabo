package hostagent

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/libracoder/kiri/engine/internal/guestwire"
)

func TestGoGuestPersistsVerifiedLayers(t *testing.T) {
	binary := os.Getenv("KIRI_TEST_GO_AGENT")
	if binary == "" {
		t.Skip("set KIRI_TEST_GO_AGENT to the compiled Go agent")
	}
	dir := shortTemp(t)
	layerDir := filepath.Join(dir, "layers")
	t.Setenv("KIRI_GO_LAYER_DIR", layerDir)
	p, err := Start(t.Context(), binary, os.Stderr)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	_ = p.Conn.SetDeadline(time.Now().Add(5 * time.Second))
	s := guestwire.NewSession(p.Conn, nil)
	blob := tarBlob(t, "etc/test", "content")
	digest := fmt.Sprintf("%x", sha256.Sum256(blob))
	if have, err := s.HasLayer(t.Context(), digest); err != nil || have {
		t.Fatalf("unexpected initial query: %v, %v", have, err)
	}
	got, err := s.SendLayer(t.Context(), digest, "application/vnd.oci.image.layer.v1.tar+gzip", bytes.NewReader(blob), int64(len(blob)))
	if err != nil || got.UnpackedBytes != 7 {
		t.Fatalf("layer upload: %+v %v", got, err)
	}
	raw, err := os.ReadFile(filepath.Join(layerDir, digest, "etc/test"))
	if err != nil || string(raw) != "content" {
		t.Fatalf("unpacked layer: %q %v", raw, err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	if err := p.Wait(); err != nil {
		t.Fatal(err)
	}
	p2, err := Start(t.Context(), binary, os.Stderr)
	if err != nil {
		t.Fatal(err)
	}
	defer p2.Close()
	_ = p2.Conn.SetDeadline(time.Now().Add(5 * time.Second))
	s2 := guestwire.NewSession(p2.Conn, nil)
	if have, err := s2.HasLayer(t.Context(), digest); err != nil || !have {
		t.Fatalf("layer unavailable after agent restart: %v %v", have, err)
	}
	bad := tarBlob(t, "../escaped", "bad")
	badDigest := fmt.Sprintf("%x", sha256.Sum256(bad))
	if _, err := s2.SendLayer(t.Context(), badDigest, "application/vnd.oci.image.layer.v1.tar+gzip", bytes.NewReader(bad), int64(len(bad))); err == nil || !strings.Contains(err.Error(), "escapes") {
		t.Fatalf("accepted escaping archive: %v", err)
	}
	if _, err := os.Stat(filepath.Join(layerDir, badDigest+".done")); !os.IsNotExist(err) {
		t.Fatalf("invalid layer was published: %v", err)
	}
	if _, err := os.Stat(filepath.Join(layerDir, "escaped")); !os.IsNotExist(err) {
		t.Fatalf("archive wrote outside the layer: %v", err)
	}
}

func tarBlob(t *testing.T, name, data string) []byte {
	t.Helper()
	var blob bytes.Buffer
	gzipWriter := gzip.NewWriter(&blob)
	tarWriter := tar.NewWriter(gzipWriter)
	h := &tar.Header{Name: name, Mode: 0644, Uid: os.Geteuid(), Gid: os.Getegid(), Size: int64(len(data))}
	if err := tarWriter.WriteHeader(h); err != nil {
		t.Fatal(err)
	}
	if _, err := tarWriter.Write([]byte(data)); err != nil {
		t.Fatal(err)
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatal(err)
	}
	return blob.Bytes()
}
