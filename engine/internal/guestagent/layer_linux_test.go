package guestagent

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// layerEntry is one tar entry for a test layer.
type layerEntry struct {
	name, link string
	kind       byte
	data       string
}

// buildLayer writes entries as a tar stream owned by the test's user, so
// untarLayer's chown also works without root.
func buildLayer(t *testing.T, entries ...layerEntry) *tar.Reader {
	t.Helper()
	var payload bytes.Buffer
	w := tar.NewWriter(&payload)
	for _, e := range entries {
		h := &tar.Header{Name: e.name, Linkname: e.link, Typeflag: e.kind, Mode: 0644, Uid: os.Getuid(), Gid: os.Getgid()}
		switch e.kind {
		case tar.TypeReg:
			h.Size = int64(len(e.data))
		case tar.TypeDir:
			h.Mode = 0755
		}
		if err := w.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(e.data)); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return tar.NewReader(&payload)
}

// sameInode reports whether a and b, not following a final symlink, are
// one file.
func sameInode(t *testing.T, a, b string) bool {
	t.Helper()
	var x, y syscall.Stat_t
	if err := syscall.Lstat(a, &x); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Lstat(b, &y); err != nil {
		t.Fatal(err)
	}
	return x.Dev == y.Dev && x.Ino == y.Ino
}

// Nix-built images (Supabase Postgres) hard-link symlinks, absolute and
// dangling ones included: the symlink itself is linked, never followed.
func TestLayerHardLinkKeepsSourceInode(t *testing.T) {
	for _, link := range []string{"", "target", "/target", "/missing-target"} {
		dir := t.TempDir()
		root, err := os.OpenRoot(dir)
		if err != nil {
			t.Fatal(err)
		}
		entries := []layerEntry{{name: "target", kind: tar.TypeReg, data: "[]"}}
		source := "target"
		if link != "" {
			source = "source"
			entries = append(entries, layerEntry{name: source, link: link, kind: tar.TypeSymlink})
		}
		entries = append(entries, layerEntry{name: "nested/manifest.nix", link: source, kind: tar.TypeLink})
		if _, err := untarLayer(root, buildLayer(t, entries...)); err != nil {
			t.Fatalf("link to %q: %v", link, err)
		}
		_ = root.Close()
		linked := filepath.Join(dir, "nested/manifest.nix")
		if !sameInode(t, filepath.Join(dir, source), linked) {
			t.Errorf("link to %q: hard link is a different file", link)
		}
		if link == "" {
			if data, err := os.ReadFile(linked); err != nil || string(data) != "[]" {
				t.Errorf("hard link to a file reads %q, %v", data, err)
			}
		} else if got, err := os.Readlink(linked); err != nil || got != link {
			t.Errorf("hard link to symlink %q reads %q, %v", link, got, err)
		}
	}
}

// A symlinked parent directory inside the layer resolves for both names.
func TestLayerHardLinkResolvesParentsInsideLayer(t *testing.T) {
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	_, err = untarLayer(root, buildLayer(t,
		layerEntry{name: "dir/file", kind: tar.TypeReg, data: "x"},
		layerEntry{name: "alias", link: "dir", kind: tar.TypeSymlink},
		layerEntry{name: "alias/copy", link: "alias/file", kind: tar.TypeLink},
	))
	if err != nil {
		t.Fatal(err)
	}
	if !sameInode(t, filepath.Join(dir, "dir/file"), filepath.Join(dir, "dir/copy")) {
		t.Error("hard link through a symlinked parent is a different file")
	}
}

// Neither name of a hard link may leave the layer, and a directory is
// never a hard link source.
func TestLayerHardLinkCannotEscape(t *testing.T) {
	for _, tt := range []struct{ source, name string }{
		{"../outside/secret", "copied"},
		{"escape/secret", "copied"},
		{"file", "escape/copied"},
		{"file", "../outside/copied"},
		{"folder", "copied"},
	} {
		base := t.TempDir()
		dir, outside := filepath.Join(base, "layer"), filepath.Join(base, "outside")
		for _, d := range []string{dir, outside} {
			if err := os.Mkdir(d, 0755); err != nil {
				t.Fatal(err)
			}
		}
		secret := filepath.Join(outside, "secret")
		if err := os.WriteFile(secret, []byte("keep"), 0644); err != nil {
			t.Fatal(err)
		}
		root, err := os.OpenRoot(dir)
		if err != nil {
			t.Fatal(err)
		}
		_, err = untarLayer(root, buildLayer(t,
			layerEntry{name: "file", kind: tar.TypeReg, data: "x"},
			layerEntry{name: "folder", kind: tar.TypeDir},
			layerEntry{name: "escape", link: outside, kind: tar.TypeSymlink},
			layerEntry{name: tt.name, link: tt.source, kind: tar.TypeLink},
		))
		_ = root.Close()
		if err == nil {
			t.Errorf("hard link %q -> %q was accepted", tt.name, tt.source)
		}
		for _, p := range []string{filepath.Join(dir, "copied"), filepath.Join(outside, "copied")} {
			if _, err := os.Lstat(p); err == nil {
				t.Errorf("hard link %q -> %q created %s", tt.name, tt.source, p)
			}
		}
		var st syscall.Stat_t
		if err := syscall.Stat(secret, &st); err != nil || st.Nlink != 1 {
			t.Errorf("hard link %q -> %q linked the outside file: %v, %d links", tt.name, tt.source, err, st.Nlink)
		}
	}
}

// docker cp accepts a hard link to a symlink too, linking the symlink.
func TestArchiveHardLinkToSymlink(t *testing.T) {
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	var payload bytes.Buffer
	w := tar.NewWriter(&payload)
	for _, h := range []*tar.Header{
		{Name: "source", Linkname: "/missing-target", Typeflag: tar.TypeSymlink, Mode: 0777},
		{Name: "copy", Linkname: "source", Typeflag: tar.TypeLink, Mode: 0777},
	} {
		if err := w.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := extractContainerArchive(root, &payload, 0); err != nil {
		t.Fatal(err)
	}
	if !sameInode(t, filepath.Join(dir, "source"), filepath.Join(dir, "copy")) {
		t.Error("archive hard link to a symlink is a different file")
	}
	if got, err := os.Readlink(filepath.Join(dir, "copy")); err != nil || got != "/missing-target" {
		t.Errorf("archive hard link reads %q, %v", got, err)
	}
}
