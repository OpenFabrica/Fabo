package main

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// Delegate=yes gives the daemon its service subtree. Move only our process
// into the host leaf so that the service cgroup can distribute controllers.
func prepareServiceCgroup() (string, error) {
	data, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return "", err
	}
	var rel string
	for line := range strings.SplitSeq(string(data), "\n") {
		if path, found := strings.CutPrefix(line, "0::"); found {
			rel = path
			break
		}
	}
	if !filepath.IsAbs(rel) || filepath.Clean(rel) != rel || !strings.HasSuffix(filepath.Base(rel), ".service") {
		return "", fmt.Errorf("a dedicated systemd cgroup v2 service is required")
	}
	return prepareCgroup(filepath.Join("/sys/fs/cgroup", rel))
}

// WSL's boot process shares the top-level hierarchy with other programs.
// Kiri uses its own subtree and moves only the Go daemon into it.
func prepareWSLCgroup() (string, error) {
	const root = "/sys/fs/cgroup"
	available := filepath.Join(root, "cgroup.controllers")
	if _, err := os.Stat(available); os.IsNotExist(err) {
		entries, err := os.ReadDir(root)
		if err != nil {
			return "", err
		}
		if len(entries) != 0 {
			return "", fmt.Errorf("WSL needs cgroup v2 at %s", root)
		}
		if err := unix.Mount("cgroup2", root, "cgroup2", unix.MS_NOSUID|unix.MS_NODEV|unix.MS_NOEXEC, ""); err != nil {
			return "", fmt.Errorf("mount WSL cgroup v2: %w", err)
		}
	}
	if err := enableCgroupControllers(root); err != nil {
		return "", err
	}
	base := filepath.Join(root, "kiri-wsl")
	if err := os.MkdirAll(base, 0755); err != nil {
		return "", err
	}
	return prepareCgroup(base)
}

func prepareCgroup(base string) (string, error) {
	host := filepath.Join(base, "kiri-host")
	if err := os.MkdirAll(host, 0755); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(host, "cgroup.procs"), []byte(strconv.Itoa(os.Getpid())), 0); err != nil {
		return "", fmt.Errorf("move daemon into its cgroup: %w", err)
	}
	if err := enableCgroupControllers(base); err != nil {
		return "", err
	}
	containers := filepath.Join(base, "kiri")
	if err := os.MkdirAll(containers, 0755); err != nil {
		return "", err
	}
	return containers, nil
}

func enableCgroupControllers(base string) error {
	available, err := os.ReadFile(filepath.Join(base, "cgroup.controllers"))
	if err != nil {
		return fmt.Errorf("read controllers at %s: %w", base, err)
	}
	var enable []string
	for _, name := range strings.Fields(string(available)) {
		if slices.Contains([]string{"cpu", "memory", "pids", "cpuset", "io"}, name) {
			enable = append(enable, "+"+name)
		}
	}
	if len(enable) != 0 {
		if err := os.WriteFile(filepath.Join(base, "cgroup.subtree_control"), []byte(strings.Join(enable, " ")), 0); err != nil {
			return fmt.Errorf("enable controllers at %s: %w", base, err)
		}
	}
	return nil
}
