package localengine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/libracoder/kiri/engine/internal/guestwire"
)

func openRustMigrationData(source *os.Root, path, destination string) (*os.Root, error) {
	if info, err := source.Stat("blobs/sha256"); err != nil || !info.IsDir() {
		return nil, fmt.Errorf("snapshot does not contain the Rust image store's blobs/sha256 directory")
	}
	path, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	path, err = filepath.EvalSymlinks(path)
	if err != nil {
		return nil, fmt.Errorf("open the offline Rust agent data directory: %w", err)
	}
	if err := checkMigrationDestination(path, destination); err != nil {
		return nil, err
	}
	data, err := os.OpenRoot(path)
	if err != nil {
		return nil, err
	}
	for _, name := range []string{"containers", "volumes"} {
		if info, err := data.Lstat(name); err != nil || !info.IsDir() {
			data.Close()
			return nil, fmt.Errorf("Rust agent data directory does not contain a real %s directory", name)
		}
	}
	if err := checkRustAgentStopped(data); err != nil {
		data.Close()
		return nil, err
	}
	return data, nil
}

func checkRustAgentStopped(data *os.Root) error {
	lock, err := data.Open("agent.lock")
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		if errors.Is(err, unix.EWOULDBLOCK) {
			return fmt.Errorf("source Rust agent is still running")
		}
		return fmt.Errorf("check Rust agent lock: %w", err)
	}
	return nil
}

func migrateRustData(ctx context.Context, source, data *os.Root, stage string, _ *Engine) error {
	if err := checkRustAgentStopped(data); err != nil {
		return err
	}
	if err := checkRustDataMounts(data.Name()); err != nil {
		return err
	}
	containers, err := readRustIndex[rustContainer](source, "containers.json")
	if err != nil {
		return err
	}
	for _, c := range containers {
		if !validBlobName(c.ID) {
			return fmt.Errorf("invalid source container ID")
		}
		src := filepath.Join("containers", c.ID, "upper")
		dst := filepath.Join(stage, "guest", "containers", c.ID, "upper")
		if _, err := copyRustTree(ctx, data, src, dst, c.StartedNS == nil); err != nil {
			return fmt.Errorf("copy container %q writable layer: %w", c.Name, err)
		}
	}
	volumes, err := readRustIndex[rustVolume](source, "volumes.json")
	if err != nil {
		return err
	}
	for _, v := range volumes {
		if !guestwire.ValidVolumeName(v.Name) {
			return fmt.Errorf("invalid source volume name")
		}
		src := filepath.Join("volumes", v.Name, "_data")
		dst := filepath.Join(stage, src)
		found, err := copyRustTree(ctx, data, src, dst, true)
		if err != nil {
			return fmt.Errorf("copy volume %q: %w", v.Name, err)
		}
		// Rust materializes a plain volume on first use. Go creates the
		// directory when it creates the metadata, so provision unused ones.
		if !found {
			if err := os.Mkdir(dst, 0755); err != nil {
				return err
			}
			if err := os.Chmod(dst, 0755); err != nil {
				return err
			}
		}
	}
	return checkRustAgentStopped(data)
}

func checkRustDataMounts(data string) error {
	info, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return err
	}
	unescape := strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`)
	for line := range strings.SplitSeq(string(info), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 {
			continue
		}
		mount := unescape.Replace(fields[4])
		for _, name := range []string{"containers", "volumes"} {
			if strings.HasPrefix(mount, filepath.Join(data, name)+string(filepath.Separator)) {
				return fmt.Errorf("source Rust data has a mounted path %q; unmount it before migration", mount)
			}
		}
	}
	return nil
}

func copyRustTree(ctx context.Context, source *os.Root, relative, dest string, optional bool) (bool, error) {
	dir := "."
	for part := range strings.SplitSeq(relative, string(filepath.Separator)) {
		dir = filepath.Join(dir, part)
		info, err := source.Lstat(dir)
		if optional && errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if !info.IsDir() {
			return false, fmt.Errorf("source entry %q must be a real directory", dir)
		}
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0700); err != nil {
		return false, err
	}
	// Explicit xattr preservation fails on unsupported metadata rather than
	// silently dropping OverlayFS opaque directories or file capabilities.
	cmd := exec.CommandContext(ctx, "/usr/bin/cp", "--recursive", "--no-dereference", "--one-file-system",
		"--preserve=mode,ownership,timestamps,links,xattr", "--reflink=auto", "--no-target-directory",
		"--", filepath.Join(source.Name(), relative), dest)
	if out, err := cmd.CombinedOutput(); err != nil {
		return false, fmt.Errorf("preserve source data: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return true, nil
}

func copyRustFile(ctx context.Context, source *os.Root, relative, dest string) error {
	info, err := source.Lstat(relative)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("snapshot entry %q must be a regular file", relative)
	}
	f, err := source.Open(relative)
	if err != nil {
		return err
	}
	defer f.Close()
	out, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, archiveContextReader{ctx, f})
	return errors.Join(err, out.Close())
}

func publishRustMigration(stage, dest string) error {
	return unix.Renameat2(unix.AT_FDCWD, stage, unix.AT_FDCWD, dest, unix.RENAME_NOREPLACE)
}
