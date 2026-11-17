// fabo-win is Fabo's Windows host. It serves the Docker API on
// \\.\pipe\fabo (and on \\.\pipe\docker_engine while no other engine
// does), carrying each connection into WSL2 over one long-lived
// `wsl.exe -- fabo bridge`; it starts the engine in WSL, runs first-run
// setup and updates, and opens fabo-desktop.exe, the window and tray,
// which talks to it on \\.\pipe\fabo-win. Upstream Kiri's kiri-win.exe,
// without the window, which Fabo's MyGo desktop provides.
//
//	fabo-win [--background] [--distro NAME] [--pipe \\.\pipe\fabo] [--socket PATH]
//
// --background (from the sign-in entry) starts without opening the window.
// Starting it again while it runs opens the window of the running one.
package main

import (
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/OpenFabrica/Fabo/engine/internal/winhost"
)

// release is this build's version, stamped by scripts/windows-bundle.sh.
var release = "0.0.0-dev"

// manifest is `name=sha256;…` for every bundle file setup uses or runs,
// stamped by scripts/windows-bundle.sh; a release build refuses to run
// them without it.
var manifest = ""

// releaseBuild is "1" in builds made by scripts/windows-bundle.sh.
var releaseBuild = ""

func main() {
	// Load DLLs only from System32, not from the folder fabo-win runs from.
	winhost.HardenDLLSearch()
	dir := winhost.Dir()
	logf := func(format string, args ...any) { winhost.Log(dir, fmt.Sprintf(format, args...)) }
	defer func() {
		// There is no console to print a panic to.
		if r := recover(); r != nil {
			logf("panic: %v", r)
			os.Exit(2)
		}
	}()
	flags := flag.NewFlagSet("fabo-win", flag.ContinueOnError)
	background := flags.Bool("background", false, "start without opening the window")
	distro := flags.String("distro", "", "the WSL distro the engine runs in (skips setup; for development)")
	pipe := flags.String("pipe", winhost.Pipe, "the named pipe to serve the Docker API on")
	socket := flags.String("socket", winhost.WSLSocket, "the engine's socket in WSL")
	if err := flags.Parse(os.Args[1:]); err != nil {
		os.Exit(2)
	}
	exe, _ := os.Executable()
	h := &host{
		dir: dir, install: filepath.Dir(exe), logger: logf,
		manifest: winhost.Manifest(manifest), release: releaseBuild == "1",
		pipe: *pipe, socket: *socket, distroFlag: *distro, quitting: make(chan struct{}),
	}
	// A staged update goes in before anything else, once: if the helper
	// left to install it failed, Fabo starts as it is and the window
	// offers the update again rather than quitting on every launch.
	h.engine = h.newEngineControl()
	h.updates = newUpdater(h)
	if h.updates.installStagedAtLaunch() {
		return
	}
	if err := h.serve(); err != nil {
		// Another fabo-win serves the pipe: open its window instead.
		if running(*pipe) {
			if !*background {
				h.openDesktop(false)
			}
			return
		}
		// Whatever holds the name is not Fabo, and docker would talk to it
		// as if it were. Say so and stop.
		logf("pipe: %v; not starting", err)
		os.Exit(1)
	}
	h.setup = newSetup(h, dir)
	h.setup.finished = func() {
		h.setupPending.Store(false)
		h.ensureEngineAsync()
	}
	h.setupPending.Store(*distro == "" && h.setup.state().Step != "done")
	go h.serveControl()
	go h.watchEngine()
	go h.pushProxy()
	go h.updates.loop()
	// The window and tray: shown, or in the tray only when Windows starts
	// Fabo at sign-in, unless setup still has to be finished.
	h.openDesktop(*background && !h.setupPending.Load())
	<-h.quitting
	// The window asked to quit: a staged update goes in now.
	h.updates.installStagedAtQuit()
}

// running reports whether a fabo-win answers on its control pipe.
func running(pipe string) bool {
	if !strings.EqualFold(pipe, winhost.Pipe) {
		return false
	}
	c := controlClient()
	resp, err := c.Get("http://fabo-win/status")
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// errNoDesktop is a development build run without fabo-desktop.exe.
var errNoDesktop = errors.New("fabo-desktop.exe is not beside fabo-win.exe")
