package localengine

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"

	"golang.org/x/sys/unix"
)

func openRustMigrationData(source *os.Root, _ string, _ string) (*os.Root, error) {
	err := checkRustDisk(source)
	if errors.Is(err, os.ErrNotExist) {
		// Pulling and scanning images does not create the guest disk.
		return nil, nil
	}
	return nil, err
}

func migrateRustData(ctx context.Context, source, _ *os.Root, stage string, e *Engine) error {
	if _, err := source.Lstat("vm/data.img"); errors.Is(err, os.ErrNotExist) {
		_, volumes := os.Stat(filepath.Join(stage, "volumes"))
		if len(e.containers) == 0 && errors.Is(volumes, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("the offline Rust VM disk is required to import containers or volumes: %w", err)
	} else if err != nil {
		return err
	}
	if err := copyRustFile(ctx, source, "vm/data.img", filepath.Join(stage, "data.img")); err != nil {
		return fmt.Errorf("clone the VM disk on the same APFS volume: %w", err)
	}
	return nil
}

func publishRustMigration(stage, dest string) error {
	return unix.RenamexNp(stage, dest, unix.RENAME_EXCL)
}

func checkRustDisk(source *os.Root) error {
	disk, err := source.Open("vm/data.img")
	if err != nil {
		return fmt.Errorf("open the offline Rust VM disk: %w", err)
	}
	defer disk.Close()
	info, err := disk.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("source VM disk must be a regular file")
	}
	var super [1024]byte
	if _, err := disk.ReadAt(super[:], 1024); err != nil {
		return fmt.Errorf("read ext4 superblock: %w", err)
	}
	const ext4Magic, ext4Errors = 0xef53, 2
	if binary.LittleEndian.Uint16(super[0x38:]) != ext4Magic {
		return fmt.Errorf("source VM disk is not a raw ext4 image")
	}
	if binary.LittleEndian.Uint16(super[0x3a:])&ext4Errors != 0 {
		return fmt.Errorf("source VM disk has filesystem errors")
	}
	// Some Rust guests sync then power off without unmounting. The
	// journal may be replayed when Go boots the independent clone.
	return nil
}

func copyRustFile(_ context.Context, source *os.Root, relative, dest string) error {
	parent, err := source.Open(path.Dir(relative))
	if err != nil {
		return err
	}
	defer parent.Close()
	info, err := source.Lstat(relative)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("snapshot entry %q must be a regular file", relative)
	}
	if err := unix.Clonefileat(int(parent.Fd()), path.Base(relative), unix.AT_FDCWD, dest, unix.CLONE_NOFOLLOW); err != nil {
		return err
	}
	return os.Chmod(dest, 0600)
}
