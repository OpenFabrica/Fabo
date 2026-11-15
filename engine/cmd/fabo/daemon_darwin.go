package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"syscall"

	"golang.org/x/sys/unix"
)

// engine is the daemon beside this executable in the app bundle.
func (i installation) engine() (string, error) {
	engine := filepath.Join(i.dir, "fabo-daemon")
	if info, err := os.Stat(engine); err != nil || !info.Mode().IsRegular() {
		return "", fmt.Errorf("no fabo-daemon in %s: run fabo from the app bundle", i.dir)
	}
	return engine, nil
}

// daemonCommand prepares the private directories and returns the engine with
// the bundle's guest, this installation's paths and the VM's default size.
func (i installation) daemonCommand() (string, []string, error) {
	engine, err := i.engine()
	if err != nil {
		return "", nil, err
	}
	root, socket := i.root(), i.socket()
	state := filepath.Join(root, "go-engine")
	run := filepath.Dir(socket)
	if err := privateDir(run); err != nil {
		return "", nil, err
	}
	if run == filepath.Join(root, "run") {
		// The Rust daemon created this directory with the user's umask, and
		// the Go daemon refuses a socket directory others can enter. Only
		// the standard path is tightened, never a custom socket directory.
		info, err := os.Lstat(run)
		if err != nil {
			return "", nil, err
		}
		owner, ok := info.Sys().(*syscall.Stat_t)
		if mode := info.Mode().Perm(); info.IsDir() && ok && owner.Uid == uint32(os.Geteuid()) && mode&0o077 != 0 && mode&0o022 == 0 {
			if err := os.Chmod(run, 0o700); err != nil {
				return "", nil, err
			}
		}
	}
	if err := privateDir(state); err != nil {
		return "", nil, err
	}

	guest := filepath.Join(i.dir, "..", "Resources", "guest")
	kernel := "Image"
	if runtime.GOARCH == "amd64" {
		kernel = "bzImage"
	}
	if _, err := os.Stat(filepath.Join(guest, kernel)); err != nil {
		return "", nil, fmt.Errorf("no guest kernel in %s: run fabo from the app bundle", guest)
	}
	memory, err := defaultMemoryMiB()
	if err != nil {
		return "", nil, err
	}
	args := []string{
		"-socket", socket,
		"-state", state,
		"-domain-tls-dir", filepath.Join(root, "tls"),
		"-share-config", filepath.Join(root, "share.json"),
		"-finder-socket", filepath.Join(root, "files", "nfs.sock"),
		"-finder-mount", filepath.Join(i.home, i.names.app),
	}
	if !i.names.flavored {
		args = append(args, "-domains-default")
	}
	return engine, append(args,
		"-kernel", filepath.Join(guest, kernel),
		"-initrd", filepath.Join(guest, "initramfs-go.cpio"),
		"-disk", filepath.Join(state, "data.img"),
		"-disk-template", filepath.Join(guest, "data.img.tgz"),
		"-cpus", strconv.Itoa(defaultCPUs(runtime.NumCPU())),
		"-memory", strconv.FormatUint(memory, 10),
		"-standard-shares",
		"-builder-container", "buildx_buildkit_"+i.names.builder+"0",
	), nil
}

// defaultCPUs is FABO_CPUS, else 2, within what the Mac has. The engine's own
// saved settings take precedence once the user changes them.
func defaultCPUs(available int) int {
	cpus, err := strconv.Atoi(os.Getenv("FABO_CPUS"))
	if err != nil {
		cpus = 2
	}
	return min(max(cpus, 1), max(available, 1))
}

// defaultMemoryMiB is FABO_MEMORY_MIB, else half the Mac's RAM between 2 and 8 GiB.
func defaultMemoryMiB() (uint64, error) {
	if mib, err := strconv.ParseUint(os.Getenv("FABO_MEMORY_MIB"), 10, 64); err == nil && mib > 0 {
		return mib, nil
	}
	physical, err := unix.SysctlUint64("hw.memsize")
	if err != nil {
		return 0, errors.Join(errors.New("cannot read the Mac's memory size"), err)
	}
	return min(max(physical/2, 2<<30), 8<<30) >> 20, nil
}
