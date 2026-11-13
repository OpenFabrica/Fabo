package guestagent

import (
	"archive/tar"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"

	"github.com/libracoder/kiri/engine/internal/guestwire"
	"golang.org/x/sys/unix"
)

func extractContainerArchive(root *os.Root, source io.Reader, flags uint32) error {
	tr := tar.NewReader(source)
	var dirs []tar.Header
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		name, err := containerArchiveName(header.Name)
		if err != nil {
			return err
		}
		header.Name = name
		if err := root.MkdirAll(filepath.Dir(name), 0755); err != nil {
			return err
		}
		directory := header.Typeflag == tar.TypeDir
		info, err := root.Lstat(name)
		if err == nil && directory != info.IsDir() {
			if flags&guestwire.CopyNoReplace != 0 || name == "." {
				return fmt.Errorf("cannot replace directory with file or the reverse: %s", name)
			}
			if err := root.RemoveAll(name); err != nil {
				return err
			}
		} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}

		if directory {
			if err := root.MkdirAll(name, 0755); err != nil {
				return err
			}
			dirs = append(dirs, *header)
			continue
		}
		if err := replaceArchiveEntry(root, tr, header, flags); err != nil {
			return fmt.Errorf("extract %s: %w", name, err)
		}
	}
	// Child writes change directory timestamps. Apply directory metadata
	// after extracting the entries.
	for _, dir := range slices.Backward(dirs) {
		f, err := root.OpenFile(dir.Name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
		if err != nil {
			return err
		}
		if err := errors.Join(applyArchiveMetadata(f, &dir, flags), f.Close()); err != nil {
			return err
		}
	}
	return nil
}

func replaceArchiveEntry(root *os.Root, source io.Reader, header *tar.Header, flags uint32) error {
	if header.Name == "." {
		return fmt.Errorf("archive file cannot replace the extraction directory")
	}
	tmp := filepath.Join(filepath.Dir(header.Name), ".kiri-copy-"+rand.Text())
	switch header.Typeflag {
	case tar.TypeReg:
		f, err := root.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		defer root.Remove(tmp)
		_, err = io.CopyN(f, source, header.Size)
		if err == nil {
			err = applyArchiveMetadata(f, header, flags)
		}
		if err := errors.Join(err, f.Close()); err != nil {
			return err
		}
	case tar.TypeSymlink:
		if err := root.Symlink(header.Linkname, tmp); err != nil {
			return err
		}
		defer root.Remove(tmp)
		if flags&guestwire.CopyKeepOwners != 0 {
			if err := root.Lchown(tmp, header.Uid, header.Gid); err != nil {
				return err
			}
		}
	case tar.TypeLink:
		target, err := containerArchiveName(header.Linkname)
		if err != nil {
			return err
		}
		info, err := root.Lstat(target)
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("hard link target must be a regular file: %s", target)
		}
		if err := root.Link(target, tmp); err != nil {
			return err
		}
		defer root.Remove(tmp)
	default:
		return fmt.Errorf("unsupported archive entry %q", header.Name)
	}
	return root.Rename(tmp, header.Name)
}

func applyArchiveMetadata(f *os.File, header *tar.Header, flags uint32) error {
	if flags&guestwire.CopyKeepOwners != 0 {
		if err := f.Chown(header.Uid, header.Gid); err != nil {
			return err
		}
	}
	mode := header.FileInfo().Mode() & (os.ModePerm | os.ModeSticky | os.ModeSetuid | os.ModeSetgid)
	if err := f.Chmod(mode); err != nil {
		return err
	}
	if !header.ModTime.IsZero() {
		mtime := unix.Timespec{Sec: header.ModTime.Unix(), Nsec: int64(header.ModTime.Nanosecond())}
		return unix.UtimesNanoAt(int(f.Fd()), "", []unix.Timespec{mtime, mtime}, unix.AT_EMPTY_PATH)
	}
	return nil
}
