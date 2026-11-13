//go:build darwin && cgo

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// prepare runs under the daemon state lock, before Virtualization opens the
// disk. The temporary directory is next to the disk so publishing is atomic.
func (o guestOptions) prepare(ctx context.Context) error {
	if o.diskTemplate == "" {
		return nil
	}
	if o.disk == "" {
		return fmt.Errorf("a data disk path is required with the disk template")
	}
	if info, err := os.Lstat(o.disk); err == nil {
		if !info.Mode().IsRegular() {
			return fmt.Errorf("VM data disk must be a regular file")
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	dir, err := os.MkdirTemp(filepath.Dir(o.disk), ".go-vm-disk-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	// The app ships a sparse archive. tar recreates the holes on disk.
	command := exec.CommandContext(ctx, "/usr/bin/tar", "-xzf", o.diskTemplate, "-C", dir, "data.img")
	command.Stderr = os.Stderr
	if err := command.Run(); err != nil {
		return fmt.Errorf("unpack the VM data disk template: %w", err)
	}
	image := filepath.Join(dir, "data.img")
	info, err := os.Lstat(image)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() == 0 {
		return fmt.Errorf("invalid VM data disk template")
	}
	if err := os.Chmod(image, 0600); err != nil {
		return err
	}
	return os.Rename(image, o.disk)
}
