package guestagent

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"
	"golang.org/x/sys/unix"

	"github.com/OpenFabrica/Fabo/engine/internal/guestwire"
)

type LayerStore struct{ dir string }

func OpenLayers(dir string) (*LayerStore, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	return &LayerStore{dir}, nil
}

func layerDigest(raw string) (string, error) {
	digest := strings.TrimPrefix(raw, "sha256:")
	if len(digest) != 64 {
		return "", fmt.Errorf("invalid layer digest")
	}
	if _, err := hex.DecodeString(digest); err != nil {
		return "", fmt.Errorf("invalid layer digest")
	}
	return strings.ToLower(digest), nil
}

func (s *LayerStore) has(digest string) bool {
	layer, err := os.Stat(filepath.Join(s.dir, digest))
	if err != nil || !layer.IsDir() {
		return false
	}
	marker, err := os.Stat(filepath.Join(s.dir, digest+".done"))
	return err == nil && marker.Mode().IsRegular()
}

func (s *LayerStore) receive(conn io.Reader, begin guestwire.LayerBegin) (guestwire.LayerStored, error) {
	if begin.Size > math.MaxInt64 {
		return guestwire.LayerStored{}, fmt.Errorf("layer size exceeds supported range")
	}
	started := time.Now()
	raw := &io.LimitedReader{R: conn, N: int64(begin.Size)}
	hash := sha256.New()
	blob := io.TeeReader(raw, hash)
	digest, err := layerDigest(begin.Digest)
	var expanded uint64
	var tmp string
	if err == nil {
		tmp, err = os.MkdirTemp(s.dir, ".layer-")
	}
	if err == nil {
		defer os.RemoveAll(tmp)
		// The store directory is private. An OCI archive without an explicit
		// root header still needs a traversable filesystem root.
		if err = os.Chmod(tmp, 0755); err == nil {
			expanded, err = unpackLayer(blob, begin.MediaType, tmp, begin.Size)
		}
	}
	// Consume the declared blob even on a local validation or unpack error.
	if _, readErr := io.Copy(io.Discard, blob); readErr != nil {
		return guestwire.LayerStored{}, readErr
	}
	if raw.N != 0 {
		return guestwire.LayerStored{}, io.ErrUnexpectedEOF
	}
	if err != nil {
		return guestwire.LayerStored{}, err
	}
	if hex.EncodeToString(hash.Sum(nil)) != digest {
		return guestwire.LayerStored{}, fmt.Errorf("layer digest mismatch")
	}
	if !s.has(digest) {
		target := filepath.Join(s.dir, digest)
		if err := os.RemoveAll(target); err != nil {
			return guestwire.LayerStored{}, err
		}
		if err := os.Rename(tmp, target); err != nil {
			return guestwire.LayerStored{}, err
		}
		marker, err := os.OpenFile(filepath.Join(s.dir, digest+".done"), os.O_CREATE|os.O_WRONLY, 0600)
		if err != nil {
			return guestwire.LayerStored{}, err
		}
		if err := errors.Join(marker.Sync(), marker.Close()); err != nil {
			_ = os.Remove(filepath.Join(s.dir, digest+".done"))
			return guestwire.LayerStored{}, err
		}
	}
	return guestwire.LayerStored{UnpackedBytes: expanded, ElapsedNS: uint64(time.Since(started))}, nil
}

func unpackLayer(blob io.Reader, media, dest string, size uint64) (uint64, error) {
	var payload io.Reader = blob
	switch {
	case strings.HasSuffix(media, "gzip"):
		r, err := gzip.NewReader(blob)
		if err != nil {
			return 0, err
		}
		defer r.Close()
		payload = r
	case strings.HasSuffix(media, "zstd"):
		r, err := zstd.NewReader(blob, zstd.WithDecoderMaxMemory(128<<20))
		if err != nil {
			return 0, err
		}
		defer r.Close()
		payload = r
	case strings.HasSuffix(media, "tar"):
	default:
		return 0, fmt.Errorf("unsupported layer media type %q", media)
	}
	capped := &io.LimitedReader{R: payload, N: guestwire.LayerExpansionLimit(size) + 1}
	root, err := os.OpenRoot(dest)
	if err != nil {
		return 0, err
	}
	defer root.Close()
	bytes, err := untarLayer(root, tar.NewReader(capped))
	if err != nil {
		return 0, err
	}
	// Finish decoding to check compression checksums and enforce the cap.
	if _, err := io.Copy(io.Discard, capped); err != nil {
		return 0, err
	}
	if capped.N == 0 {
		return 0, fmt.Errorf("layer expands beyond its allowed size")
	}
	return bytes, nil
}

func archivePath(raw string) (string, error) {
	if raw == "" || path.IsAbs(raw) {
		return "", fmt.Errorf("invalid layer path %q", raw)
	}
	for part := range strings.SplitSeq(raw, "/") {
		if part == ".." {
			return "", fmt.Errorf("layer path escapes archive root: %q", raw)
		}
	}
	return path.Clean(raw), nil
}

// untarLayer unpacks an image layer's entries under root and returns the
// bytes of regular file data it wrote.
func untarLayer(root *os.Root, archive *tar.Reader) (uint64, error) {
	dirs := make(map[string]tar.Header)
	var bytes uint64
	for {
		h, err := archive.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return 0, err
		}
		name, err := archivePath(h.Name)
		if err != nil {
			return 0, err
		}
		if name == "." && h.Typeflag != tar.TypeDir {
			return 0, fmt.Errorf("invalid layer root entry")
		}
		if err := root.MkdirAll(path.Dir(name), 0755); err != nil {
			return 0, err
		}
		if hidden, ok := strings.CutPrefix(path.Base(name), ".wh."); ok {
			if err := whiteout(root, path.Dir(name), hidden); err != nil {
				return 0, err
			}
			continue
		}
		if h.Typeflag == tar.TypeDir {
			if err := root.MkdirAll(name, 0755); err != nil {
				return 0, err
			}
			dirs[name] = *h
			continue
		}
		if err := root.RemoveAll(name); err != nil {
			return 0, err
		}
		delete(dirs, name)
		switch h.Typeflag {
		case tar.TypeReg:
			if err := extractFile(root, name, h, archive); err != nil {
				return 0, err
			}
			bytes += uint64(h.Size)
		case tar.TypeSymlink:
			if err := root.Symlink(h.Linkname, name); err != nil {
				return 0, err
			}
			if err := root.Lchown(name, h.Uid, h.Gid); err != nil {
				return 0, err
			}
		case tar.TypeLink:
			target, err := archivePath(h.Linkname)
			if err != nil {
				return 0, err
			}
			info, err := root.Lstat(target)
			if err != nil || !hardLinkable(info.Mode()) {
				return 0, fmt.Errorf("layer hard link target is not a regular file or symlink: %q", target)
			}
			// Root.Link calls linkat without AT_SYMLINK_FOLLOW, so a symlink
			// source is linked itself and its target, possibly absolute or
			// dangling, is never resolved (Nix-built images hard-link those).
			if err := root.Link(target, name); err != nil {
				return 0, err
			}
		case tar.TypeFifo:
			if err := extractFIFO(root, name, h); err != nil {
				return 0, err
			}
		default:
			return 0, fmt.Errorf("unsupported archive entry %q (%d)", name, h.Typeflag)
		}
	}
	names := make([]string, 0, len(dirs))
	for name := range dirs {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool { return len(names[i]) > len(names[j]) })
	for _, name := range names {
		f, err := root.Open(name)
		if err != nil {
			return 0, err
		}
		h := dirs[name]
		if err := errors.Join(applyMetadata(f, &h), f.Close()); err != nil {
			return 0, err
		}
	}
	return bytes, nil
}

// hardLinkable reports whether a tar hard link may name a file of mode as
// its source: a regular file, or a symlink, which is linked itself.
func hardLinkable(mode os.FileMode) bool {
	return mode.IsRegular() || mode&os.ModeSymlink != 0
}

func extractFIFO(root *os.Root, name string, h *tar.Header) error {
	parent, err := root.Open(path.Dir(name))
	if err != nil {
		return err
	}
	defer parent.Close()
	if err := unix.Mkfifoat(int(parent.Fd()), path.Base(name), 0600); err != nil {
		return err
	}
	f, err := root.OpenFile(name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	return applyMetadata(f, h)
}

func extractFile(root *os.Root, name string, h *tar.Header, data io.Reader) error {
	f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := io.Copy(f, data); err != nil {
		return err
	}
	if err := applyMetadata(f, h); err != nil {
		return err
	}
	return f.Sync()
}

func applyMetadata(f *os.File, h *tar.Header) error {
	if err := f.Chown(h.Uid, h.Gid); err != nil {
		return err
	}
	if err := f.Chmod(h.FileInfo().Mode()); err != nil {
		return err
	}
	for key, value := range h.PAXRecords {
		if attr, ok := strings.CutPrefix(key, "SCHILY.xattr."); ok {
			if err := unix.Fsetxattr(int(f.Fd()), attr, []byte(value), 0); err != nil {
				return err
			}
		}
	}
	if !h.ModTime.IsZero() {
		ts := unix.NsecToTimeval(h.ModTime.UnixNano())
		return unix.Futimes(int(f.Fd()), []unix.Timeval{ts, ts})
	}
	return nil
}

func whiteout(root *os.Root, dir, hidden string) error {
	if hidden == "" || hidden == "." || hidden == ".." {
		return fmt.Errorf("invalid layer whiteout")
	}
	current := "."
	for part := range strings.SplitSeq(dir, "/") {
		current = path.Join(current, part)
		info, err := root.Lstat(current)
		if err != nil || !info.IsDir() {
			return fmt.Errorf("whiteout parent is not a real directory: %q", current)
		}
	}
	f, err := root.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	if hidden == ".wh..opq" {
		return unix.Fsetxattr(int(f.Fd()), "trusted.overlay.opaque", []byte("y"), 0)
	}
	if err := root.RemoveAll(path.Join(dir, hidden)); err != nil {
		return err
	}
	return unix.Mknodat(int(f.Fd()), hidden, unix.S_IFCHR, 0)
}
