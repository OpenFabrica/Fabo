package guestagent

import (
	"archive/tar"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"strings"
	"syscall"

	"github.com/libracoder/kiri/engine/internal/guestwire"
	"golang.org/x/sys/unix"
)

func sendWritableLayer(conn io.Writer, cfg copyConfig) error {
	upper, err := os.OpenRoot(cfg.Overlay.Upper)
	if err != nil {
		copyReply(conn, err)
		return err
	}
	defer upper.Close()
	chunked := cfg.Spec.Flags&guestwire.CopyChunked != 0
	if cfg.Spec.Op == guestwire.CopyCommit {
		return streamCopyResult(conn, chunked, func(w io.Writer) error { return writeDeltaArchive(w, upper) })
	}
	if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""); err != nil {
		copyReply(conn, err)
		return err
	}
	lower := *cfg.Overlay
	lower.Upper, lower.Work = "", ""
	if err := mountOverlay(lower, unix.MS_NODEV|unix.MS_NOSUID); err != nil {
		copyReply(conn, err)
		return err
	}
	base, err := os.OpenRoot(lower.Root)
	if err != nil {
		copyReply(conn, err)
		return err
	}
	defer base.Close()
	changes, err := writableChanges(upper, base)
	if err != nil {
		copyReply(conn, err)
		return err
	}
	return streamCopyResult(conn, chunked, func(w io.Writer) error {
		if chunked {
			return json.NewEncoder(w).Encode(changes)
		}
		for _, change := range changes {
			if _, err := fmt.Fprintf(w, "%c %s\n", "CAD"[change.Kind], change.Path); err != nil {
				return err
			}
		}
		return nil
	})
}

func walkWritable(root *os.Root, visit func(string, fs.DirEntry, bool) error) error {
	return fs.WalkDir(root.FS(), ".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if name == "." || entry.Type()&os.ModeSocket != 0 {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		whiteout := info.Mode()&os.ModeCharDevice != 0 && info.Sys().(*syscall.Stat_t).Rdev == 0
		if info.Mode().IsRegular() && info.Size() == 0 {
			whiteout, err = overlayMarker(root, name, "whiteout")
			if err != nil {
				return err
			}
		}
		return visit(name, entry, whiteout)
	})
}

func overlayMarker(root *os.Root, name, key string) (bool, error) {
	f, err := root.OpenFile(name, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW, 0)
	if err != nil {
		return false, err
	}
	defer f.Close()
	var value [1]byte
	size, err := unix.Fgetxattr(int(f.Fd()), "trusted.overlay."+key, value[:])
	if errors.Is(err, unix.ENODATA) || errors.Is(err, unix.EOPNOTSUPP) {
		return false, nil
	}
	return err == nil && (key == "whiteout" || size == 1 && value[0] == 'y'), err
}

func writableChanges(upper, base *os.Root) ([]guestwire.FilesystemChange, error) {
	changes := []guestwire.FilesystemChange{}
	err := walkWritable(upper, func(name string, _ fs.DirEntry, whiteout bool) error {
		kind := 2
		if !whiteout {
			switch _, err := base.Lstat(name); {
			case err == nil:
				kind = 0
			case errors.Is(err, fs.ErrNotExist), errors.Is(err, unix.ENOTDIR):
				kind = 1
			default:
				return err
			}
		}
		changes = append(changes, guestwire.FilesystemChange{Path: "/" + name, Kind: kind})
		return nil
	})
	return changes, err
}

func writeDeltaArchive(dst io.Writer, upper *os.Root) error {
	tw := tar.NewWriter(dst)
	links := make(map[[2]uint64]string)
	marker := func(name string) error {
		return tw.WriteHeader(&tar.Header{Name: name, Mode: 0644, Typeflag: tar.TypeReg})
	}
	err := walkWritable(upper, func(name string, entry fs.DirEntry, whiteout bool) error {
		if whiteout {
			return marker(path.Join(path.Dir(name), ".wh."+path.Base(name)))
		}
		if strings.HasPrefix(path.Base(name), ".wh.") {
			return fmt.Errorf("cannot encode OCI reserved filename %q", name)
		}
		if entry.Type()&os.ModeDevice != 0 {
			return fmt.Errorf("device node %q is unsupported in an image layer", name)
		}
		if err := appendArchiveEntry(tw, upper, name, entry, links); err != nil {
			return err
		}
		if entry.IsDir() {
			opaque, err := overlayMarker(upper, name, "opaque")
			if err != nil {
				return err
			}
			if opaque {
				return marker(path.Join(name, ".wh..wh..opq"))
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	return tw.Close()
}
