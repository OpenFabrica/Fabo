package guestagent

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/OpenFabrica/Fabo/engine/internal/guestwire"
)

func TestArchiveDoesNotReplaceDirectoryWithFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "item"), 0755); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	var payload bytes.Buffer
	writer := tar.NewWriter(&payload)
	if err := writer.WriteHeader(&tar.Header{Name: "item", Typeflag: tar.TypeReg, Mode: 0644}); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := extractContainerArchive(root, &payload, guestwire.CopyNoReplace); err == nil {
		t.Fatal("archive replaced a directory with a file")
	}
	if info, err := root.Stat("item"); err != nil || !info.IsDir() {
		t.Fatalf("directory was changed: %v, %v", info, err)
	}
}

func TestArchivePreservesFileModeAndTimestamp(t *testing.T) {
	root, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	modified := time.Unix(1_750_000_000, 345678901)
	var payload bytes.Buffer
	writer := tar.NewWriter(&payload)
	if err := writer.WriteHeader(&tar.Header{Name: "item", Typeflag: tar.TypeReg, Mode: 0640, ModTime: modified, Format: tar.FormatPAX}); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := extractContainerArchive(root, &payload, 0); err != nil {
		t.Fatal(err)
	}
	if info, err := root.Stat("item"); err != nil || info.Mode().Perm() != 0640 || !info.ModTime().Equal(modified) {
		t.Fatalf("file metadata was changed: %v, %v", info, err)
	}
}
