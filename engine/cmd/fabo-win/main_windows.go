// fabo-win is Fabo's Windows host. It serves the Docker API on
// \\.\pipe\fabo (and on \\.\pipe\docker_engine while no other engine
// does), carrying each connection into WSL2 over one long-lived
// `wsl.exe -- fabo bridge`; it starts the engine in WSL, runs first-run
// setup and updates, and opens fabo-desktop.exe, the window and tray,
// which talks to it on \\.\pipe\fabo-win. Upstream Kiri's kiri-win.exe,
// without the window, which Fabo's MyGo desktop provides.
//
//	fabo-win [--background] [--distro NAME] [--pipe \\.\pipe\fabo] [--socket PATH]
//	fabo-win --uninstall-builds
//
// --background (from the sign-in entry) starts without opening the window.
// --uninstall-builds (from fabo.ps1 when uninstalling) gives docker build
// its own builder back as it was before Fabo took it, and exits.
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
	"time"

	"github.com/OpenFabrica/Fabo/engine/internal/buildwith"
	"github.com/OpenFabrica/Fabo/engine/internal/hostrelay"
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
	uninstall := flags.Bool("uninstall-builds", false, "give docker build its own builder back, and exit (uninstalling)")
	if err := flags.Parse(os.Args[1:]); err != nil {
		os.Exit(2)
	}
	places := func() (buildwith.Places, error) { return buildwith.WindowsPlaces(os.Getenv, dir) }
	// Uninstalling: this alone, before anything that would start Fabo.
	if *uninstall {
		os.Exit(uninstallBuilds(places, logf))
	}
	exe, _ := os.Executable()
	h := &host{
		dir: dir, install: filepath.Dir(exe), logger: logf,
		manifest: winhost.Manifest(manifest), release: releaseBuild == "1",
		pipe: *pipe, socket: *socket, distroFlag: *distro, quitting: make(chan struct{}), pipeLooked: make(chan struct{}),
		proxyKick: make(chan struct{}, 1),
	}
	h.relay = &hostrelay.Client{Start: h.startRelay, Logf: logf, Ended: h.kickProxy}
	// A staged update goes in before anything else, once: if the helper
	// left to install it failed, Fabo starts as it is and the window
	// offers the update again rather than quitting on every launch.
	h.engine = h.newEngineControl()
	h.builds = &builds{places: places, other: h.otherBuildEngine, logf: logf}
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
	// The *.fabo.local answerer, once this is the only fabo-win: a second
	// one has quit above without touching the port. Without the port Fabo
	// still runs; names stay at *.fabo.localhost.
	h.names = newLocalNames(h)
	if err := h.names.start(); err != nil {
		logf("dns on 127.0.0.1:53: %v", err)
	}
	h.setup = newSetup(h, dir)
	h.setup.finished = func() {
		h.setupPending.Store(false)
		// setup-tools.ps1 has made Fabo's builder by now.
		h.builds.apply()
		h.ensureEngineAsync()
	}
	h.usb = newUSBManager(winUSB{h})
	h.setupPending.Store(*distro == "" && h.setup.state().Step != "done")
	// Which builder docker build uses, as chosen; taken again if quitting
	// gave it back. While setup runs, setup-tools.ps1 owns docker's config.
	// It waits for serveDockerEngine's first look, so an unknown engine
	// holding docker_engine counts as another engine.
	if !h.setupPending.Load() {
		go func() {
			select {
			case <-h.pipeLooked:
			case <-time.After(5 * time.Second):
			}
			h.builds.apply()
		}()
	}
	// fabo-win's folder renamed, moved or deleted around it: docker build
	// handed back, the choice kept for when Fabo runs again, and quit.
	if exe != "" {
		watchProgram(exe, 30*time.Second, func() {
			logf("%s is gone from where Fabo started; handing docker build back and quitting", exe)
			h.builds.giveBack("program gone", false)
			h.quit()
		})
	}
	go h.serveControl()
	go h.watchEngine()
	go h.pushProxy()
	go h.updates.loop()
	// The window and tray: shown, or in the tray only when Windows starts
	// Fabo at sign-in, unless setup still has to be finished.
	h.openDesktop(*background && !h.setupPending.Load())
	<-h.quitting
	// Nothing in WSL may keep a port leading to Windows once Fabo is gone.
	// Bounded: a relay whose WSL end stopped reading would block the frame
	// write (and the lock withdrawProxy takes) forever, and quitting must
	// still hand docker build back and install a staged update. On exit
	// the relay's wsl.exe loses its stdin and ends anyway.
	withdrawn := make(chan struct{})
	go func() { h.withdrawProxy(); close(withdrawn) }()
	select {
	case <-withdrawn:
	case <-time.After(10 * time.Second):
		logf("quit: the proxy relay did not stop in 10s; quitting without it")
	}
	// Nothing serves Fabo's builder once this process has gone: docker
	// build gets its own back until Fabo starts again.
	h.builds.giveBack("quit", true)
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
