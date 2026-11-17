package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// TestBuild keeps the base's entries (as root, setuid intact), replaces a
// file Fabo provides, and adds the rest with their parent directories.
func TestBuild(t *testing.T) {
	dir := t.TempDir()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	add := func(h *tar.Header, body string) {
		h.Size = int64(len(body))
		tw.WriteHeader(h)
		tw.Write([]byte(body))
	}
	add(&tar.Header{Typeflag: tar.TypeDir, Name: "./etc/", Mode: 0o755, Uid: 1000}, "")
	add(&tar.Header{Typeflag: tar.TypeReg, Name: "./bin/bbsuid", Mode: 0o4755, Uid: 1000}, "suid")
	add(&tar.Header{Typeflag: tar.TypeReg, Name: "./etc/wsl.conf", Mode: 0o644}, "old")
	add(&tar.Header{Typeflag: tar.TypeSymlink, Name: "./bin/sh", Linkname: "/bin/busybox"}, "")
	tw.Close()
	zw.Close()
	base := filepath.Join(dir, "base.tar.gz")
	os.WriteFile(base, buf.Bytes(), 0o644)
	bin := filepath.Join(dir, "fabo")
	os.WriteFile(bin, []byte("ELF"), 0o644)
	out := filepath.Join(dir, "out.tar.gz")
	auto := "auto\n"
	conf := "new"
	err := build(base, out, []entry{
		{name: "usr/local/bin/fabo", src: bin, mode: 0o755},
		{name: "etc/fabo/wsl-engine", text: &auto, mode: 0o644},
		{name: "etc/wsl.conf", text: &conf, mode: 0o644},
	})
	if err != nil {
		t.Fatal(err)
	}
	f, _ := os.Open(out)
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]*tar.Header{}
	bodies := map[string]string{}
	var order []string
	tr := tar.NewReader(zr)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(tr)
		if _, dup := got[h.Name]; dup {
			t.Fatalf("%s twice", h.Name)
		}
		got[h.Name], bodies[h.Name] = h, string(b)
		order = append(order, h.Name)
	}
	if h := got["bin/bbsuid"]; h == nil || h.Mode != 0o4755 || h.Uid != 0 {
		t.Fatalf("bbsuid = %+v", h)
	}
	if h := got["bin/sh"]; h == nil || h.Linkname != "/bin/busybox" {
		t.Fatalf("symlink = %+v", h)
	}
	if bodies["etc/wsl.conf"] != "new" || bodies["etc/fabo/wsl-engine"] != "auto\n" || bodies["usr/local/bin/fabo"] != "ELF" {
		t.Fatalf("bodies = %v", bodies)
	}
	if got["usr/local/bin/fabo"].Mode != 0o755 {
		t.Fatal("mode")
	}
	for _, d := range []string{"usr/", "usr/local/", "usr/local/bin/", "etc/fabo/"} {
		if got[d] == nil {
			t.Fatalf("missing directory %s in %v", d, order)
		}
	}
}
