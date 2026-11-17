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

	"github.com/OpenFabrica/Fabo/engine/sharetunnel"
)

// release is this build's version, stamped from the git tag by the Makefile
// and scripts/macos-app.sh (docs/release.md). Plain `go build` keeps the default.
var release = "0.0.0-dev"

const usage = `usage: fabo [ui [-view PAGE] [-start]]   open the desktop window
       fabo --version                    this build's version
       fabo COMMAND                     docker commands on this engine: run, ps, exec, compose, ...
       fabo daemon                       run the engine in the foreground
       fabo setup [--use] [--no-agent]   docker context, commands on PATH, start at login, build builder
       fabo setup --uninstall            take all of that back; data stays
       fabo build-with [docker|fabo]     which builder plain docker build uses
       fabo domains [local|https on|off] container names at *.fabo.local, and HTTPS for them
       fabo engine restart    restart the engine on the installed version; containers with a restart policy start again
       fabo machine create NAME [--distro D] [--systemd] | list | shell NAME [-- CMD...]
       fabo machine start|stop|restart|delete NAME
       fabo k8s start [--use] | stop | status | delete | set [--lan on|off] [--tls-san NAME,…] [--port N]
       fabo import [orbstack|desktop-linux] [--list | --json | image|volume|container NAME] | --sources
       fabo context [use NAME]           which engine plain docker uses (macOS)
       fabo debug CONTAINER              a shell with debugging tools beside any running container
       fabo pack [-o FILE] [--project NAME] [CONTAINER...]   containers with their changes and volumes, in one file
       fabo unpack [--start] FILE        recreate packed containers here
       fabo usb [list] | attach DEVICE | detach DEVICE | helper remove
       fabo share PORT|CONTAINER [NAME]  a public HTTPS link to it; also fabo PORT [NAME]
       fabo share login [--server HOST] [--token T] | logout | status | upgrade | names | reserve|release NAME
       fabo bench [--runs N] [--image IMAGE] [--network none|bridge] [--only fabo|orbstack|docker]
       fabo raw run [OPTIONS] IMAGE [COMMAND...] | pull IMAGE | images | rmi IMAGE...
       fabo migrate [OPTIONS]       import an offline Kiri store
       fabo mcp                          read-only Model Context Protocol server for agents, on stdin/stdout
       fabo bridge [--socket PATH]       (in WSL) carry fabo-win.exe's Docker API connections to the engine
       fabo hostrelay                    (in WSL) carry connections to a proxy on Windows' loopback for fabo-win.exe
`

func main() {
	args := os.Args[1:]
	var err error
	switch {
	case len(args) == 1 && (args[0] == "--help" || args[0] == "-h"):
		fmt.Print(usage)
		return
	case len(args) == 1 && (args[0] == "--version" || args[0] == "-v"):
		fmt.Println("fabo", release)
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
	case args[0] == "debug":
		err = debugCommand(args[1:])
	case args[0] == "pack":
		err = packCommand(args[1:])
	case args[0] == "unpack":
		err = unpackCommand(args[1:])
	case args[0] == "k8s":
		err = k8sCommand(args[1:])
	case args[0] == "import":
		err = importCommand(args[1:])
	case args[0] == "context":
		err = contextCommand(args[1:])
	case args[0] == "usb":
		err = usbCommand(args[1:])
	case args[0] == "share":
		err = shareCommand(args[1:])
	case args[0] == "bench":
		err = benchCommand(args[1:])
	case args[0] == "raw":
		err = rawCommand(args[1:])
	case args[0] == "machine":
		err = machineCommand(args[1:])
	case args[0] == "bridge":
		err = bridgeCommand(args[1:])
	case args[0] == "hostrelay":
		err = hostrelayCommand(args[1:])
	case args[0] == "engine":
		err = engineCommand(args[1:])
	case handlesDocker(args[0]):
		var in installation
		in, err = locate()
		if err == nil {
			err = in.runDocker(args)
		}
	case sharetunnel.IsShorthand(args[0]):
		// `fabo 3000` shares a port; no docker command looks like one.
		err = shareCommand(args)
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
	// sharesEngine is set by FaboSharesEngine in a development bundle's
	// Info.plist: its window uses the installed app's engine, containers
	// and images instead of starting its own.
	sharesEngine bool
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
	return installation{dir: dir, home: home, names: namesFor(bundleID(string(plist))), sharesEngine: plistTrue(string(plist), "FaboSharesEngine")}, nil
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
