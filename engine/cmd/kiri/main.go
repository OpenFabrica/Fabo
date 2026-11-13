//go:build linux || darwin

// kiri is the entry point of the app bundle and the Linux packages. It
// starts the Go engine with the installation's paths and opens the desktop.
package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

const usage = `usage: kiri [ui [-view PAGE] [-start]]   open the desktop window
       kiri COMMAND                     docker commands on this engine: run, ps, exec, compose, ...
       kiri daemon                       run the engine in the foreground
       kiri setup [--use] [--no-agent]   docker context, commands on PATH, start at login, build builder
       kiri setup --uninstall            take all of that back; data stays
       kiri build-with [docker|kiri]     which builder plain docker build uses
       kiri domains [local|https on|off] container names at *.kiri.local, and HTTPS for them
       kiri migrate-rust [OPTIONS]       import an offline Kiri (Rust) store
`

func main() {
	args := os.Args[1:]
	var err error
	switch {
	case len(args) == 0 || args[0] == "ui":
		err = openDesktop(args)
	case args[0] == "daemon":
		err = runDaemon()
	case args[0] == "setup":
		err = setupCommand(args[1:])
	case args[0] == "build-with":
		err = buildWithCommand(args[1:])
	case args[0] == "domains":
		err = domainsCommand(args[1:])
	case args[0] == "migrate-rust":
		err = migrate(args)
	case handlesDocker(args[0]):
		var in installation
		in, err = locate()
		if err == nil {
			err = in.runDocker(args)
		}
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "kiri:", err)
		// Only for the window: the LaunchAgent's `kiri daemon` is restarted
		// when it fails, and must not raise a dialog each time.
		if len(args) == 0 {
			reportFailure(err)
		}
		os.Exit(1)
	}
}

// installation is where this executable lives and the names it runs under.
type installation struct {
	dir   string // directory holding kiri, with symlinks resolved
	home  string
	names names
}

// locate resolves the running executable and the names its bundle selects.
func locate() (installation, error) {
	exe, err := os.Executable()
	if err != nil {
		return installation{}, err
	}
	if exe, err = filepath.EvalSymlinks(exe); err != nil {
		return installation{}, err
	}
	home := os.Getenv("HOME")
	if home == "" {
		return installation{}, errors.New("HOME is not set")
	}
	dir := filepath.Dir(exe)
	plist, _ := os.ReadFile(filepath.Join(dir, "..", "Info.plist"))
	return installation{dir: dir, home: home, names: namesFor(bundleID(string(plist)))}, nil
}

// root is the installation's directory under HOME: socket, logs and Go state.
func (i installation) root() string { return filepath.Join(i.home, i.names.state) }

// socket is the Docker API socket: KIRI_SOCKET, else <root>/run/docker.sock.
func (i installation) socket() string {
	if socket := os.Getenv("KIRI_SOCKET"); socket != "" {
		return socket
	}
	return filepath.Join(i.root(), "run", "docker.sock")
}

// runDaemon replaces this process with the engine.
func runDaemon() error {
	in, err := locate()
	if err != nil {
		return err
	}
	engine, args, err := in.daemonCommand()
	if err != nil {
		return err
	}
	return syscall.Exec(engine, append([]string{engine}, args...), os.Environ())
}

// migrate hands migrate-rust to the engine, which owns the import.
func migrate(args []string) error {
	in, err := locate()
	if err != nil {
		return err
	}
	engine, err := in.engine()
	if err != nil {
		return err
	}
	return syscall.Exec(engine, append([]string{engine}, args...), os.Environ())
}

// privateDir creates dir, and any missing parents, for the daemon user only.
func privateDir(dir string) error { return os.MkdirAll(dir, 0o700) }
