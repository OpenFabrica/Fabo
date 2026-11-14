package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	libexec   = "/usr/libexec/fabo"
	wslEngine = "/etc/fabo/wsl-engine"
)

// engineDir holds the daemon and agent: beside this executable in a build
// tree, /usr/libexec/fabo in a package or a Fabo Linux distro.
func (i installation) engineDir() string {
	if info, err := os.Stat(filepath.Join(i.dir, "fabo-daemon")); err == nil && info.Mode().IsRegular() {
		return i.dir
	}
	return libexec
}

// engine is the daemon executable, which must exist.
func (i installation) engine() (string, error) {
	engine := filepath.Join(i.engineDir(), "fabo-daemon")
	if info, err := os.Stat(engine); err != nil || !info.Mode().IsRegular() {
		return "", fmt.Errorf("no fabo-daemon in %s", filepath.Dir(engine))
	}
	return engine, nil
}

// daemonCommand prepares the private directories and returns the engine with
// its host-process agent and the cgroup mode of this host.
func (i installation) daemonCommand() (string, []string, error) {
	engine, err := i.engine()
	if err != nil {
		return "", nil, err
	}
	agent := filepath.Join(filepath.Dir(engine), "fabo-init")
	if info, err := os.Stat(agent); err != nil || !info.Mode().IsRegular() {
		return "", nil, fmt.Errorf("no fabo-init in %s", filepath.Dir(agent))
	}
	if _, err := os.Stat("/sys/fs/cgroup/cgroup.controllers"); err != nil {
		return "", nil, errors.New("the engine needs cgroup v2")
	}
	wsl := false
	if choice, err := os.ReadFile(wslEngine); err == nil {
		wsl = true
		// A Linux distro records its engine here. "go" and "auto" both run
		// this engine; anything else names an engine this build lacks.
		switch choice := strings.TrimSpace(string(choice)); choice {
		case "go", "auto":
		default:
			return "", nil, fmt.Errorf("%s selects the %q engine, which this build does not include", wslEngine, choice)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", nil, err
	}
	socket := i.socket()
	state := filepath.Join(i.root(), "go-engine")
	run := filepath.Dir(socket)
	if err := privateDir(run); err != nil {
		return "", nil, err
	}
	if err := privateDir(state); err != nil {
		return "", nil, err
	}
	if wsl && run == "/var/lib/fabo-daemon/.fabo/run" {
		// The Rust service used the distro's umask for this directory.
		// The WSL bridge runs as root, so it needs no group access.
		if err := os.Chmod(run, 0o700); err != nil {
			return "", nil, err
		}
	}
	// systemd sets INVOCATION_ID for every unit. Only Fabo's own unit, or a
	// WSL distro's, delegates the cgroup subtree that -systemd expects.
	service := os.Getenv("INVOCATION_ID") != "" && (wsl || os.Getenv("STATE_DIRECTORY") != "")
	args := []string{"-agent", agent, "-socket", socket, "-state", state, "-domains-default"}
	if wsl {
		args = append(args, "-windows-proxy")
	}
	if service {
		args = append(args, "-systemd")
	} else if wsl {
		args = append(args, "-wsl")
	}
	return engine, args, nil
}
