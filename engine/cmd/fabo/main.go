//go:build linux || darwin

// fabo is the entry point of the app bundle and the Linux packages. It
// starts the Go engine with the installation's paths and opens the desktop.
package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

const usage = `usage: fabo [ui [-view PAGE] [-start]]   open the desktop window
       fabo COMMAND                     docker commands on this engine: run, ps, exec, compose, ...
       fabo daemon                       run the engine in the foreground
       fabo setup [--use] [--no-agent]   docker context, commands on PATH, start at login, build builder
       fabo setup --uninstall            take all of that back; data stays
       fabo build-with [docker|fabo]     which builder plain docker build uses
       fabo domains [local|https on|off] container names at *.fabo.local, and HTTPS for them
       fabo machine create NAME [--distro D] [--systemd] | list | shell NAME [-- CMD...]
       fabo machine start|stop|restart|delete NAME
       fabo k8s start [--use] | stop | status | delete | set [--lan on|off] [--tls-san NAME,…] [--port N]
       fabo import [orbstack|desktop-linux] [--list | image|volume|container NAME]
       fabo usb [list] | attach DEVICE | detach DEVICE | helper remove
       fabo bench [--runs N] [--image IMAGE] [--network none|bridge] [--only fabo|orbstack|docker]
       fabo raw run [OPTIONS] IMAGE [COMMAND...] | pull IMAGE | images | rmi IMAGE...
       fabo migrate [OPTIONS]       import an offline Kiri store
       fabo mcp                          read-only Model Context Protocol server for agents, on stdin/stdout
`

func main() {
	args := os.Args[1:]
	var err error
	switch {
	case len(args) == 1 && (args[0] == "--help" || args[0] == "-h"):
		fmt.Print(usage)
		return
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
	case args[0] == "migrate":
		err = migrate(args)
	case args[0] == "mcp":
		err = mcpCommand(args[1:])
	case args[0] == "k8s":
		err = k8sCommand(args[1:])
	case args[0] == "import":
		err = importCommand(args[1:])
	case args[0] == "usb":
		err = usbCommand(args[1:])
	case args[0] == "bench":
		err = benchCommand(args[1:])
	case args[0] == "raw":
		err = rawCommand(args[1:])
	case args[0] == "machine":
		err = machineCommand(args[1:])
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
		fmt.Fprintln(os.Stderr, "fabo:", err)
		// Only for the window: the LaunchAgent's `fabo daemon` is restarted
		// when it fails, and must not raise a dialog each time.
		if len(args) == 0 {
			reportFailure(err)
		}
		os.Exit(1)
	}
}

// installation is where this executable lives and the names it runs under.
type installation struct {
	dir   string // directory holding fabo, with symlinks resolved
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

// socket is the Docker API socket: FABO_SOCKET, else <root>/run/docker.sock.
func (i installation) socket() string {
	if socket := os.Getenv("FABO_SOCKET"); socket != "" {
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

// migrate hands migrate to the engine, which owns the import.
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
