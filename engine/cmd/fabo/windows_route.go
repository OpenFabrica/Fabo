package main

import "slices"

// The `fabo` command on Windows (bin\fabo.exe) runs nothing itself: docker
// commands go to the bundled docker.exe on Fabo's pipe, the engine's own
// commands to the Linux `fabo` in the WSL distro the engine runs in, and
// window and engine controls to fabo-win. The routing is here, without a
// build tag, so it is tested everywhere; main_windows.go carries it out.
// Upstream Kiri's crates/kiri/src/wsl.rs.

// windowsRoute is where the Windows `fabo` sends a command.
type windowsRoute int

const (
	routeUsage   windowsRoute = iota
	routeVersion              // fabo --version
	routeWindow               // fabo, fabo ui: open Fabo (fabo-win)
	routeDocker               // docker commands, through docker.exe
	routeWSL                  // the engine's commands, in the WSL distro
	routeShell                // fabo machine shell NAME: docker exec here
	routeEngine               // fabo engine restart|start|stop: fabo-win
	routeUnsupported
)

// wslCommands run as root in the engine's distro, as the engine runs, so
// they find its socket.
var wslCommands = []string{"k8s", "machine", "usb", "import", "raw", "share", "bench"}

// macOnly are the launcher's commands that have no Windows meaning.
var macOnly = []string{"setup", "build-with", "domains", "context", "daemon", "migrate", "bridge", "debug", "pack", "unpack", "mcp"}

// routeWindows decides where args go.
func routeWindows(args []string) windowsRoute {
	if len(args) == 0 || args[0] == "ui" {
		return routeWindow
	}
	switch args[0] {
	case "--version", "-v":
		return routeVersion
	case "--help", "-h", "help":
		return routeUsage
	case "engine":
		if len(args) == 2 && slices.Contains([]string{"restart", "start", "stop"}, args[1]) {
			return routeEngine
		}
		return routeUsage
	case "machine":
		if len(args) >= 3 && args[1] == "shell" {
			return routeShell
		}
		return routeWSL
	}
	if slices.Contains(wslCommands, args[0]) {
		return routeWSL
	}
	if slices.Contains(macOnly, args[0]) {
		return routeUnsupported
	}
	if windowsDocker(args[0]) {
		return routeDocker
	}
	return routeUsage
}

// windowsDocker is the launcher's docker command list (handlesDocker),
// update included: Windows has no `fabo update` of its own.
func windowsDocker(command string) bool {
	return slices.Contains([]string{"attach", "build", "builder", "buildx", "commit", "compose", "container", "cp", "create", "diff",
		"events", "exec", "export", "history", "image", "images", "info", "inspect", "kill", "load", "login", "logout", "logs",
		"manifest", "network", "pause", "port", "ps", "pull", "push", "rename", "restart", "rm", "rmi", "run", "save", "search",
		"start", "stats", "stop", "system", "tag", "top", "unpause", "update", "version", "volume", "wait"}, command)
}

// wslEngineScript starts the engine if it is not up (Fabo Linux's
// fabo-start; a distro with systemd runs it as a service), then runs the
// Linux `fabo` with the engine's HOME, so it finds the daemon's socket.
const wslEngineScript = `export HOME=/var/lib/fabo-daemon; [ -x /usr/local/bin/fabo-start ] && ! [ -d /run/systemd/system ] && /usr/local/bin/fabo-start >/dev/null 2>&1; exec /usr/local/bin/fabo "$@"`

// wslArgs is the wsl.exe command line for an engine command in distro.
func wslArgs(distro string, args []string) []string {
	return append([]string{"-d", distro, "-u", "root", "-e", "sh", "-c", wslEngineScript, "fabo"}, args...)
}

// windowsShellArgs is `docker exec` for `fabo machine shell NAME [-- CMD…]`:
// the machine runs as a container, and Fabo Linux has no docker CLI, so
// the shell opens from Windows. bash when the machine has it, else sh.
func windowsShellArgs(name string, rest []string, tty bool) []string {
	flag := "-i"
	if tty {
		flag = "-it"
	}
	// The machine was made by the Linux fabo as root in WSL: its shell is root's.
	args := []string{"exec", flag, "-u", "root", "-w", "/root", "-e", "HOME=/root"}
	if tty {
		args = append(args, "-e", "TERM=xterm-256color")
	}
	args = append(args, "machine-"+name)
	if len(rest) > 1 && rest[0] == "--" {
		return append(args, rest[1:]...)
	}
	return append(args, "sh", "-c", "exec $(command -v bash || command -v sh) -l")
}

// windowsUsage is the Windows `fabo` command's help.
const windowsUsage = `usage: fabo [ui]                         open Fabo
       fabo --version                    this build's version
       fabo COMMAND                     docker commands on Fabo: run, ps, exec, compose, ...
       fabo engine start|stop|restart    the engine in WSL; restart also installs this version's engine
       fabo machine create NAME [--distro D] [--systemd] | list | shell NAME [-- CMD...]
       fabo machine start|stop|restart|delete NAME
       fabo k8s start [--use] | stop | status | delete | set [--lan on|off] [--tls-san NAME,…] [--port N]
       fabo usb [list] | attach DEVICE | detach DEVICE
       fabo share PORT|CONTAINER [NAME]  a public HTTPS link to it
       fabo raw run [OPTIONS] IMAGE [COMMAND...] | pull IMAGE | images | rmi IMAGE...
`
