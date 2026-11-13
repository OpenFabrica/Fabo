package guestagent

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"slices"
	"strconv"
)

func copyUpVolume(imageFD, volumeFD int) error {
	image, err := os.OpenRoot("/proc/self/fd/" + strconv.Itoa(imageFD))
	if err != nil {
		return err
	}
	defer image.Close()
	volume, err := os.OpenRoot("/proc/self/fd/" + strconv.Itoa(volumeFD))
	if err != nil {
		return err
	}
	defer volume.Close()
	files, err := fs.ReadDir(volume.FS(), ".")
	if err != nil || len(files) != 0 {
		return err
	}
	var dirs []string
	err = fs.WalkDir(image.FS(), ".", func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		switch {
		case info.IsDir():
			dirs = append(dirs, name)
			if name == "." {
				return nil
			}
			return volume.Mkdir(name, 0700)
		case info.Mode().IsRegular():
			return copyVolumeFile(image, volume, name, info)
		case info.Mode()&os.ModeSymlink != 0:
			link, err := image.Readlink(name)
			if err != nil {
				return err
			}
			if err := volume.Symlink(link, name); err != nil {
				return err
			}
			h, err := tar.FileInfoHeader(info, link)
			if err != nil {
				return err
			}
			return volume.Lchown(name, h.Uid, h.Gid)
		default:
			return fmt.Errorf("unsupported file type at %q", name)
		}
	})
	if err != nil {
		return err
	}
	// Apply directory ownership and mode only after creating descendants.
	slices.Reverse(dirs)
	for _, name := range dirs {
		info, err := image.Stat(name)
		if err != nil {
			return err
		}
		f, err := volume.Open(name)
		if err != nil {
			return err
		}
		if err := errors.Join(copyVolumeMetadata(f, info), f.Close()); err != nil {
			return err
		}
	}
	return nil
}

func copyVolumeFile(image, volume *os.Root, name string, info fs.FileInfo) (err error) {
	src, err := image.Open(name)
	if err != nil {
		return err
	}
	defer src.Close()
	dst, err := volume.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, dst.Close()) }()
	if _, err := io.Copy(dst, src); err != nil {
		return err
	}
	return copyVolumeMetadata(dst, info)
}

func copyVolumeMetadata(file *os.File, info fs.FileInfo) error {
	header, err := tar.FileInfoHeader(info, "")
	if err != nil {
		return err
	}
	return applyMetadata(file, header)
}
