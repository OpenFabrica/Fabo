//go:build linux || darwin

// Package diskusage measures the allocated blocks of engine storage trees.
package diskusage

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"syscall"
)

// Allocated counts blocks without following symlinks, crossing devices, or
// counting a hard-linked inode twice. The walk stays within the opened root.
func Allocated(ctx context.Context, path string) (uint64, error) {
	root, err := os.OpenRoot(path)
	if err != nil {
		return 0, err
	}
	defer root.Close()
	top, err := root.Stat(".")
	if err != nil {
		return 0, err
	}
	dev := top.Sys().(*syscall.Stat_t).Dev
	seen := make(map[uint64]bool)
	var bytes uint64
	err = fs.WalkDir(root.FS(), ".", func(_ string, entry fs.DirEntry, err error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if errors.Is(err, fs.ErrNotExist) {
			return nil // the tree can change while a container runs
		}
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if errors.Is(err, fs.ErrNotExist) {
			if entry.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if err != nil {
			return err
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			return fmt.Errorf("file block statistics are unavailable")
		}
		if stat.Dev != dev {
			if entry.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if !entry.IsDir() && stat.Nlink > 1 {
			inode := uint64(stat.Ino)
			if seen[inode] {
				return nil
			}
			seen[inode] = true
		}
		bytes += uint64(stat.Blocks) * 512
		return nil
	})
	return bytes, err
}
