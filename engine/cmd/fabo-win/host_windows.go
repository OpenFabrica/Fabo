package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sys/windows/registry"

	"github.com/OpenFabrica/Fabo/engine/internal/buildwith"
	"github.com/OpenFabrica/Fabo/engine/internal/hostrelay"
	"github.com/OpenFabrica/Fabo/engine/internal/winhost"
	"github.com/OpenFabrica/Fabo/engine/internal/wslbridge"
)

// otherEngines serve docker_engine themselves: while one runs the pipe is
// theirs, and Fabo lets go of it (Docker Desktop cannot start its own
// while any instance of Fabo's is open).
var otherEngines = []string{"Docker Desktop.exe", "com.docker.backend.exe", "dockerd.exe", "Rancher Desktop.exe"}

// setupUnfinished is what docker prints while setup has not finished.
const setupUnfinished = "Fabo's setup isn't finished: open Fabo to finish it"

// host is the running fabo-win.
type host struct {
	dir, install string
	logger       func(string, ...any)
	manifest     winhost.Manifest
	release      bool
	pipe, socket string
	distroFlag   string

	mux          *wslbridge.Mux
	setup        *setup
	updates      *updater
	names        *localNames
	usb          *usbManager
	setupPending atomic.Bool
	engine       *engineControl
	builds       *builds
	dockerEngine atomic.Bool
	otherEngine  atomic.Value // string
	// pipeHeld: docker_engine is served by something Fabo did not
	// recognise (it could not take the name).
	pipeHeld atomic.Bool
	// pipeLooked is closed once serveDockerEngine has first looked at who
	// serves docker_engine.
	pipeLooked chan struct{}
	lookedOnce sync.Once
	// otherCLIOnce and otherCLISeen cache otherCLI.
	otherCLIOnce sync.Once
	otherCLISeen bool

	// relay carries connections from WSL to a proxy on Windows' loopback.
	relay *hostrelay.Client
	// proxyKick asks pushProxy to push now; proxyMu keeps pushes in order.
	proxyKick chan struct{}
	proxyMu   sync.Mutex

	quitOnce sync.Once
	quitting chan struct{}
}

// logf writes to fabo-win.log.
func (h *host) logf(format string, args ...any) { h.logger(format, args...) }

// distro is the WSL distro the engine runs in: --distro, else setup's.
func (h *host) distro() string {
	if h.distroFlag != "" {
		return h.distroFlag
	}
	return winhost.LoadSaved(h.dir).Distro
}

// serve starts the Docker API on the pipe. It fails if another process
// already serves the name.
func (h *host) serve() error {
	l, err := winhost.ListenPipe(h.pipe)
	if err != nil {
		return err
	}
	h.mux = &wslbridge.Mux{
		Start: h.startBridge,
		Refuse: func(c wslbridge.Conn) bool {
			if !h.setupPending.Load() {
				return false
			}
			return wslbridge.RefuseHTTP(c, setupUnfinished)
		},
		Logf: h.logf,
	}
	h.logf("serving %s (%s)", h.pipe, h.distro())
	go func() {
		// Start WSL now, so the first docker command does not wait for it.
		if err := h.mux.Warm(); err != nil && !errors.Is(err, wslbridge.ErrNotSetUp) {
			h.logf("bridge: %v", err)
		}
	}()
	go h.acceptLoop(l)
	go h.serveDockerEngine()
	return nil
}

// acceptLoop hands each client of l to the bridge. Nothing here may take
// the whole Windows side down.
func (h *host) acceptLoop(l net.Listener) {
	for {
		c, err := l.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			h.logf("%s: %v", h.pipe, err)
			time.Sleep(100 * time.Millisecond)
			continue
		}
		go h.mux.Serve(c.(wslbridge.Conn))
	}
}

// serveDockerEngine serves docker_engine whenever it is free, and hands it
// back when another engine starts. Clients connected then keep their
// connections until they close.
func (h *host) serveDockerEngine() {
	said := ""
	say := func(msg string) {
		if msg != said {
			h.logf("%s", msg)
			said = msg
		}
	}
	looked := func() { h.lookedOnce.Do(func() { close(h.pipeLooked) }) }
	for {
		if other := winhost.Running(otherEngines...); other != "" {
			h.otherEngine.Store(other)
			looked()
			say(fmt.Sprintf("%s left to %s", winhost.DockerEnginePipe, other))
			time.Sleep(5 * time.Second)
			continue
		}
		h.otherEngine.Store("")
		l, err := winhost.ListenPipe(winhost.DockerEnginePipe)
		h.pipeHeld.Store(err != nil)
		looked()
		if err != nil {
			say(fmt.Sprintf("%s left to whatever serves it: %v", winhost.DockerEnginePipe, err))
			time.Sleep(5 * time.Second)
			continue
		}
		say(fmt.Sprintf("serving %s as well (no other engine has it)", winhost.DockerEnginePipe))
		h.dockerEngine.Store(true)
		stop := make(chan struct{})
		go func() {
			for {
				select {
				case <-stop:
					return
				case <-time.After(2 * time.Second):
				}
				if other := winhost.Running(otherEngines...); other != "" {
					h.logf("%s started: handing %s back", other, winhost.DockerEnginePipe)
					l.Close()
					return
				}
			}
		}()
		h.acceptLoop(l)
		close(stop)
		h.dockerEngine.Store(false)
	}
}

// startBridge runs `wsl.exe -d <distro> -u root -- fabo bridge`.
func (h *host) startBridge() (*wslbridge.Transport, error) {
	distro := h.distro()
	if distro == "" {
		return nil, wslbridge.ErrNotSetUp
	}
	c := winhost.Quiet("wsl.exe", "-d", distro, "-u", "root", "--", winhost.WSLFabo, "bridge", "--socket", h.socket)
	in, err := c.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := c.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := c.Start(); err != nil {
		return nil, err
	}
	return &wslbridge.Transport{In: in, Out: bufio.NewReaderSize(out, 64<<10), Wait: c.Wait}, nil
}

// wsl is a quiet `wsl.exe -d <distro> -u root -- args…`.
func (h *host) wsl(distro string, args ...string) *exec.Cmd {
	return winhost.Quiet("wsl.exe", append([]string{"-d", distro, "-u", "root", "--"}, args...)...)
}

// wslRun runs args as root in distro and reports WSL's first line on failure.
func (h *host) wslRun(distro string, args ...string) error {
	out, err := h.wsl(distro, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s", winhost.FirstLine(out, "WSL failed: "+err.Error()))
	}
	return nil
}

// toWSL is path as distro sees it.
func (h *host) toWSL(distro, path string) (string, error) {
	out, err := winhost.Quiet("wsl.exe", "-d", distro, "--", "wslpath", "-a", strings.ReplaceAll(path, `\`, "/")).Output()
	if p := strings.TrimSpace(string(out)); err == nil && p != "" {
		return p, nil
	}
	return "", fmt.Errorf("WSL could not see %s", path)
}

// engineAlive asks the engine for /_ping through Fabo's own pipe.
func (h *host) engineAlive() bool {
	c := &http.Client{
		Timeout: 3 * time.Second,
		Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return winhost.DialPipe(h.pipe, 2*time.Second)
		}},
	}
	resp, err := c.Get("http://docker/_ping")
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// syncEngine makes the engine binaries in the distro this install's,
// copying only what differs (install.sh --sync); nothing running stops.
// Without it, an update would leave the old engine in WSL for good.
func (h *host) syncEngine(distro string) (bool, error) {
	for _, f := range []string{"fabo", "fabo-daemon", "fabo-init", "fabo-start"} {
		if _, err := h.manifest.Verified(h.install, f, h.release); err != nil {
			return false, err
		}
	}
	script, err := h.manifest.Verified(h.install, "install.sh", h.release)
	if err != nil {
		return false, err
	}
	if _, err := os.Stat(script); err != nil {
		return false, nil // a development build run from elsewhere
	}
	path, err := h.toWSL(distro, script)
	if err != nil {
		return false, err
	}
	out, err := h.wsl(distro, "sh", path, "--sync").CombinedOutput()
	if err != nil {
		return false, fmt.Errorf("%s", winhost.FirstLine(out, "could not copy the engine into WSL"))
	}
	return strings.Contains(string(out), "copied"), nil
}

// newEngineControl wires the engine's operations in WSL to one lock.
func (h *host) newEngineControl() *engineControl {
	return &engineControl{alive: h.engineAlive, start: h.startInWSL, stop: h.stopInWSL, restart: h.restartInWSL, shutdown: h.shutdownWSL}
}

// ensureEngine starts the engine unless it answers (setup's engine step).
func (h *host) ensureEngine() error { return h.engine.ensure(false) }

// ensureEngineAsync starts the engine in the background unless the user
// stopped it.
func (h *host) ensureEngineAsync() {
	go func() {
		if err := h.engine.ensure(true); err != nil {
			h.logf("engine: %v", err)
		}
	}()
}

// startEngine is the window's Start.
func (h *host) startEngine() error { return h.engine.userStart() }

// stopEngine is the window's Stop: the engine stops in WSL and stays
// stopped until Start, though fabo-win keeps serving the pipe.
func (h *host) stopEngine() error { return h.engine.userStop() }

// restartEngine puts this install's engine into WSL and restarts it.
func (h *host) restartEngine() error { return h.engine.userRestart() }

// startInWSL starts the engine: the systemd service in a distro with
// systemd, fabo-start in Fabo Linux, after syncing this install's binaries.
func (h *host) startInWSL() error {
	distro := h.distro()
	if distro == "" {
		return errors.New("Fabo is not set up yet")
	}
	h.logf("engine: not answering; starting it in WSL")
	if copied, err := h.syncEngine(distro); err != nil {
		h.logf("engine: could not copy the engine into WSL (%v); starting the one there", err)
	} else if copied {
		h.logf("engine: copied this version's engine into WSL")
	}
	if h.wslRun(distro, "systemctl", "start", "fabo") != nil {
		if err := h.wslRun(distro, winhost.WSLStart); err != nil {
			h.logf("engine: fabo-start: %v", err)
		}
	}
	for range 100 {
		if h.engineAlive() {
			// The engine keeps Windows' proxy in memory only.
			h.kickProxy()
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return errors.New("Fabo's engine did not start in WSL; see fabo-win.log")
}

// stopInWSL stops the engine in WSL.
func (h *host) stopInWSL() error {
	distro := h.distro()
	if distro == "" {
		return errors.New("Fabo is not set up yet")
	}
	if h.wslRun(distro, "systemctl", "stop", "fabo") != nil {
		return h.wslRun(distro, winhost.WSLStart, "stop")
	}
	return nil
}

// restartInWSL syncs this install's engine into WSL and restarts it, and
// the bridge, which then runs the new `fabo`. Running containers stop;
// those with a restart policy start again.
func (h *host) restartInWSL() error {
	distro := h.distro()
	if distro == "" {
		return errors.New("Fabo is not set up yet")
	}
	if _, err := h.syncEngine(distro); err != nil {
		return err
	}
	if h.wslRun(distro, "systemctl", "restart", "fabo") != nil {
		if err := h.wslRun(distro, winhost.WSLStart, "restart"); err != nil {
			return err
		}
	}
	h.mux.Reset()
	// The relay too, so it also runs the `fabo` just synced; the push that
	// follows starts it again and gives the new engine its ports.
	h.relay.Close()
	for range 100 {
		if h.engineAlive() {
			h.kickProxy()
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return errors.New("Fabo's engine did not come back after the restart")
}

// watchEngine keeps the engine running while setup is done and the user
// has not stopped it: WSL shuts a distro down when nothing uses it, and
// after Windows restarts nothing else would start it.
func (h *host) watchEngine() {
	for {
		select {
		case <-h.quitting:
			return
		default:
		}
		if !h.setupPending.Load() && h.distro() != "" {
			if err := h.engine.ensure(true); err != nil {
				h.logf("engine: %v", err)
			}
		}
		time.Sleep(15 * time.Second)
	}
}

// systemProxy reads Windows' proxy for the current user.
func systemProxy() winhost.SystemProxy {
	k, err := registry.OpenKey(registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\Internet Settings`, registry.QUERY_VALUE)
	if err != nil {
		return winhost.ParseSystemProxy(false, "", "", false)
	}
	defer k.Close()
	enable, _, _ := k.GetIntegerValue("ProxyEnable")
	server, _, _ := k.GetStringValue("ProxyServer")
	override, _, _ := k.GetStringValue("ProxyOverride")
	pac, _, err := k.GetStringValue("AutoConfigURL")
	return winhost.ParseSystemProxy(enable == 1, server, override, err == nil && pac != "")
}

// pushProxy tells the engine what Windows' proxy is, now, every minute,
// and whenever kickProxy asks: the engine keeps it in memory only, so this
// follows Windows' setting and survives an engine restart.
func (h *host) pushProxy() {
	for {
		h.syncProxy()
		select {
		case <-h.quitting:
			return
		case <-h.proxyKick:
		case <-time.After(time.Minute):
		}
	}
}

// kickProxy makes pushProxy push soon (after the engine starts, after the
// relay ended under it), without waiting for it.
func (h *host) kickProxy() {
	select {
	case h.proxyKick <- struct{}{}:
	default:
	}
}

// syncProxy pushes Windows' proxy to the engine once. A proxy on Windows'
// own loopback (Windows' setting, or Settings → Network's custom one) is
// reached through the relay (upstream crates/kiri-win/src/relay.rs): the
// relay listens for its port in WSL and the engine gets the port map. One
// the relay cannot reach is left out, so pulls connect directly.
func (h *host) syncProxy() {
	h.proxyMu.Lock()
	defer h.proxyMu.Unlock()
	select {
	case <-h.quitting:
		// withdrawProxy has run or is about to: start nothing again.
		return
	default:
	}
	distro := h.distro()
	// Without an engine there is nobody to tell, and starting the relay
	// would only keep WSL running.
	if distro == "" || !h.engineAlive() {
		return
	}
	sys := systemProxy()
	targets := hostrelay.Targets(append(sys.Addresses(), h.customProxy()...)...)
	if len(targets) == 0 {
		// Withdraw first, then stop the relay, so the engine never holds a
		// port that no longer leads anywhere.
		h.putProxy(sys, nil)
		h.relay.Close()
		return
	}
	h.putProxy(sys, h.relay.Ports(distro, targets))
}

// putProxy sends PUT /fabo/proxy/system.
func (h *host) putProxy(sys winhost.SystemProxy, relay map[uint16]uint16) {
	if err := h.engineRequest(http.MethodPut, "/fabo/proxy/system", winhost.ProxyUpdate(sys, relay)); err != nil {
		h.logf("proxy: %v", err)
	}
}

// customProxy is Settings → Network's proxy when it is the one in use:
// the engine relays a custom proxy on Windows' loopback too.
func (h *host) customProxy() []string {
	var v struct {
		Settings struct {
			Mode  string `json:"mode"`
			HTTP  string `json:"http"`
			HTTPS string `json:"https"`
		} `json:"settings"`
	}
	if h.engineGet("/fabo/proxy", &v) != nil || v.Settings.Mode != "custom" {
		return nil
	}
	return []string{v.Settings.HTTP, v.Settings.HTTPS}
}

// withdrawProxy is fabo-win quitting: the engine is told the relay is gone
// (keeping proxies it can reach on its own), and the relay stops.
func (h *host) withdrawProxy() {
	h.proxyMu.Lock()
	defer h.proxyMu.Unlock()
	if h.distro() != "" && h.engineAlive() {
		h.putProxy(systemProxy(), nil)
	}
	h.relay.Close()
}

// refreshProxy is the control API's POST /proxy: the window saved proxy
// settings, and a custom proxy on Windows' loopback needs the relay now.
func (h *host) refreshProxy() { h.syncProxy() }

// startRelay runs `wsl.exe -d <distro> -u root -- fabo hostrelay`. Its
// stderr is discarded: stdout carries only frames.
func (h *host) startRelay(distro string) (*hostrelay.Process, error) {
	c := winhost.Quiet("wsl.exe", "-d", distro, "-u", "root", "--", winhost.WSLFabo, "hostrelay")
	in, err := c.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := c.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := c.Start(); err != nil {
		return nil, err
	}
	return &hostrelay.Process{
		In: in, Out: bufio.NewReaderSize(out, 64<<10),
		Kill: func() { _ = c.Process.Kill() },
		Wait: c.Wait,
	}, nil
}

// engineGet reads one JSON answer from the engine through Fabo's pipe.
func (h *host) engineGet(path string, out any) error {
	c := &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return winhost.DialPipe(h.pipe, 2*time.Second)
		}},
	}
	resp, err := c.Get("http://docker" + path)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", path, resp.Status)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(out)
}

// engineRequest sends one request to the engine through Fabo's pipe.
func (h *host) engineRequest(method, path string, body []byte) error {
	c := &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return winhost.DialPipe(h.pipe, 2*time.Second)
		}},
	}
	req, err := http.NewRequest(method, "http://docker"+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("%s %s: %s", method, path, strings.TrimSpace(string(msg)))
	}
	return nil
}

// status is GET /status.
func (h *host) status() hostStatus {
	st := hostStatus{Version: release, SetupPending: h.setupPending.Load(), Distro: h.distro(), DockerEngine: h.dockerEngine.Load()}
	if other, _ := h.otherEngine.Load().(string); other != "" {
		st.OtherEngine = other
	}
	if st.Engine = h.engine.current(); st.Engine == "" {
		st.Engine = "stopped"
		if h.engineAlive() {
			st.Engine = "running"
		}
	}
	return st
}

// setupState and setupAction are the control API's setup endpoints.
func (h *host) setupState() setupView { return h.setup.state() }

// setupAction runs one setup action.
func (h *host) setupAction(action string, value json.RawMessage) error {
	return h.setup.action(action, value)
}

// buildWith is GET /buildwith: which builder docker build uses.
func (h *host) buildWith() buildwith.Status { return h.builds.status() }

// chooseBuildWith is the window's choice of builder for docker build.
func (h *host) chooseBuildWith(with string) error { return h.builds.choose(with) }

// otherCLI is otherDocker, looked at once: PATH does not change under a
// running process, and the window asks for the status often.
func (h *host) otherCLI() bool {
	h.otherCLIOnce.Do(func() { h.otherCLISeen = h.otherDocker() })
	return h.otherCLISeen
}

// otherBuildEngine names the engine besides Fabo for the build-with
// question: one installed, else what holds docker_engine instead of Fabo.
// The pipe is never opened to find out (that would take one of its
// server's instances); serveDockerEngine has seen who holds it.
func (h *host) otherBuildEngine() string {
	other, _ := h.otherEngine.Load().(string)
	held := buildwith.WindowsEngineName(other)
	// A docker CLI of its own on PATH (setup's otherDocker) points at some
	// other engine too: asked, as upstream's setup asks, not taken unasked.
	if held == "" && (h.pipeHeld.Load() || h.otherCLI()) {
		held = "Docker"
	}
	return buildwith.WindowsOtherEngine(os.Getenv, func(p string) bool {
		info, err := os.Stat(p)
		return err == nil && info.Mode().IsRegular()
	}, held)
}

// updateState and updateAction are the control API's update endpoints.
func (h *host) updateState() updateView { return h.updates.state() }

// updateAction runs one update action.
func (h *host) updateAction(action string) error { return h.updates.action(action) }

// domains reads *.fabo.local's state for the control API.
func (h *host) domains() domainsView { return h.names.view() }

// setDomains turns *.fabo.local on or off for the control API.
func (h *host) setDomains(on bool) error { return h.names.set(on) }

// quit ends fabo-win: docker commands stop working until Fabo opens again.
func (h *host) quit() {
	h.quitOnce.Do(func() {
		h.logf("quitting")
		close(h.quitting)
	})
}

// serveControl serves the control API on its pipe.
func (h *host) serveControl() {
	l, err := winhost.ListenPipe(winhost.ControlPipe)
	if err != nil {
		h.logf("%s: %v", winhost.ControlPipe, err)
		return
	}
	srv := &http.Server{Handler: controlHandler(h), ReadHeaderTimeout: 10 * time.Second}
	_ = srv.Serve(l)
}

// controlClient is an HTTP client for a running fabo-win's control pipe.
func controlClient() *http.Client {
	return &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return winhost.DialPipe(winhost.ControlPipe, 2*time.Second)
		}},
	}
}

// openDesktop starts fabo-desktop.exe, the window and tray, with the
// `fabo` command it runs for engine commands; hidden starts it in the tray.
// A window already open comes forward (or, hidden, stays as it is).
func (h *host) openDesktop(hidden bool) {
	desktop := filepath.Join(h.install, "fabo-desktop.exe")
	if _, err := os.Stat(desktop); err != nil {
		h.logf("window: %v", errNoDesktop)
		return
	}
	c := exec.Command(desktop)
	if hidden {
		c.Args = append(c.Args, "-hidden")
	}
	c.Dir = h.install
	c.Env = append(os.Environ(), "FABO_PIPE="+h.pipe, "FABO_LAUNCHER="+filepath.Join(h.install, "bin", "fabo.exe"))
	if err := c.Start(); err != nil {
		h.logf("window: %v", err)
		return
	}
	go c.Wait()
}
