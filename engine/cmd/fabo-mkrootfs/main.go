// fabo-mkrootfs makes Fabo Linux, the WSL distro Fabo's engine runs in on
// Windows: Alpine's minirootfs with Fabo's files added, written as a new
// archive without extracting the old one. Extracting as an ordinary user
// (and on macOS's case-insensitive disk) would lose root ownership and
// setuid bits; streaming keeps every Alpine entry as it was, owned by root.
//
//	fabo-mkrootfs -base alpine-minirootfs.tar.gz -o fabo-linux.tar.gz \
//	    -add usr/local/bin/fabo=PATH:755 -text etc/fabo/wsl-engine=auto:644 ...
package main

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"
)

// entry is a file Fabo adds: its content from a file or given text.
type entry struct {
	name string
	src  string
	text *string
	mode int64
}

// list collects repeated -add or -text flags.
type list struct {
	text    bool
	entries *[]entry
}

// String is required by flag.Value.
func (l list) String() string { return "" }

// Set parses dest=value:mode.
func (l list) Set(v string) error {
	dest, rest, ok := strings.Cut(v, "=")
	i := strings.LastIndex(rest, ":")
	if !ok || i < 0 {
		return fmt.Errorf("want dest=value:mode, not %q", v)
	}
	mode, err := strconv.ParseInt(rest[i+1:], 8, 32)
	if err != nil {
		return fmt.Errorf("mode in %q: %w", v, err)
	}
	e := entry{name: clean(dest), mode: mode}
	if l.text {
		t := strings.ReplaceAll(rest[:i], `\n`, "\n")
		e.text = &t
	} else {
		e.src = rest[:i]
	}
	*l.entries = append(*l.entries, e)
	return nil
}

// clean is a tar member name without ./ or a leading slash.
func clean(name string) string {
	return strings.TrimPrefix(path.Clean("/"+name), "/")
}

func main() {
	base := flag.String("base", "", "Alpine's minirootfs .tar.gz")
	out := flag.String("o", "", "the archive to write (.tar.gz)")
	var entries []entry
	flag.Var(list{entries: &entries}, "add", "dest=file:mode, repeatable")
	flag.Var(list{text: true, entries: &entries}, "text", `dest=text:mode (\n for newlines), repeatable`)
	flag.Parse()
	if *base == "" || *out == "" {
		fmt.Fprintln(os.Stderr, "usage: fabo-mkrootfs -base ALPINE.tar.gz -o OUT.tar.gz [-add dest=file:mode] [-text dest=text:mode]")
		os.Exit(2)
	}
	if err := build(*base, *out, entries); err != nil {
		fmt.Fprintln(os.Stderr, "fabo-mkrootfs:", err)
		os.Exit(1)
	}
}

// build copies base's entries, skipping those Fabo replaces, then adds
// Fabo's directories and files, all owned by root.
func build(base, out string, entries []entry) (err error) {
	in, err := os.Open(base)
	if err != nil {
		return err
	}
	defer in.Close()
	zr, err := gzip.NewReader(in)
	if err != nil {
		return err
	}
	f, err := os.Create(out + ".tmp")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			f.Close()
			os.Remove(out + ".tmp")
		}
	}()
	zw, _ := gzip.NewWriterLevel(f, gzip.BestCompression)
	tw := tar.NewWriter(zw)
	replaced := map[string]bool{}
	for _, e := range entries {
		replaced[e.name] = true
	}
	dirs := map[string]bool{}
	tr := tar.NewReader(zr)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		name := clean(h.Name)
		if name == "" || replaced[name] {
			continue
		}
		if h.Typeflag == tar.TypeDir {
			dirs[name] = true
			name += "/"
		}
		h.Name = name
		h.Uid, h.Gid, h.Uname, h.Gname = 0, 0, "root", "root"
		h.Format = tar.FormatPAX
		if err := tw.WriteHeader(h); err != nil {
			return err
		}
		if _, err := io.Copy(tw, tr); err != nil {
			return err
		}
	}
	now := time.Now().Truncate(time.Second)
	// Parent directories Alpine does not have, shallow ones first.
	var missing []string
	for _, e := range entries {
		for d := path.Dir(e.name); d != "." && d != "/" && !dirs[d]; d = path.Dir(d) {
			dirs[d] = true
			missing = append(missing, d)
		}
	}
	sort.Strings(missing)
	for _, d := range missing {
		if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: d + "/", Mode: 0o755, Uname: "root", Gname: "root", ModTime: now, Format: tar.FormatPAX}); err != nil {
			return err
		}
	}
	for _, e := range entries {
		var data []byte
		if e.text != nil {
			data = []byte(*e.text)
		} else if data, err = os.ReadFile(e.src); err != nil {
			return err
		}
		h := &tar.Header{Typeflag: tar.TypeReg, Name: e.name, Mode: e.mode, Size: int64(len(data)), Uname: "root", Gname: "root", ModTime: now, Format: tar.FormatPAX}
		if err := tw.WriteHeader(h); err != nil {
			return err
		}
		if _, err := tw.Write(data); err != nil {
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	if err := zw.Close(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(out+".tmp", out)
}
