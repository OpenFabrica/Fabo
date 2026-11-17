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
	setupPending atomic.Bool
	// engineState is what the window shows while a start or stop runs.
	engineMu     sync.Mutex
	engineState  string
	userStopped  atomic.Bool
	dockerEngine atomic.Bool
	otherEngine  atomic.Value // string

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
	for {
		if other := winhost.Running(otherEngines...); other != "" {
			h.otherEngine.Store(other)
			say(fmt.Sprintf("%s left to %s", winhost.DockerEnginePipe, other))
			time.Sleep(5 * time.Second)
			continue
		}
		h.otherEngine.Store("")
		l, err := winhost.ListenPipe(winhost.DockerEnginePipe)
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

// ensureEngine starts the engine in WSL unless it answers: the systemd
// service in a distro with systemd, fabo-start in Fabo Linux.
func (h *host) ensureEngine() error {
	if h.engineAlive() {
		return nil
	}
	distro := h.distro()
	if distro == "" {
		return errors.New("Fabo is not set up yet")
	}
	h.setEngineState("starting")
	defer h.setEngineState("")
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
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return errors.New("Fabo's engine did not start in WSL; see Settings → Diagnostics")
}

// ensureEngineAsync starts the engine in the background.
func (h *host) ensureEngineAsync() {
	go func() {
		if err := h.ensureEngine(); err != nil {
			h.logf("engine: %v", err)
		}
	}()
}

// setEngineState records a start or stop in progress ("" when none).
func (h *host) setEngineState(state string) {
	h.engineMu.Lock()
	h.engineState = state
	h.engineMu.Unlock()
}

// startEngine is the window's Start.
func (h *host) startEngine() error {
	h.userStopped.Store(false)
	return h.ensureEngine()
}

// stopEngine is the window's Stop: the engine stops in WSL and stays
// stopped until Start, though fabo-win keeps serving the pipe.
func (h *host) stopEngine() error {
	distro := h.distro()
	if distro == "" {
		return errors.New("Fabo is not set up yet")
	}
	h.userStopped.Store(true)
	h.setEngineState("stopping")
	defer h.setEngineState("")
	if h.wslRun(distro, "systemctl", "stop", "fabo") != nil {
		if err := h.wslRun(distro, winhost.WSLStart, "stop"); err != nil {
			return err
		}
	}
	return nil
}

// restartEngine puts this install's engine into WSL and restarts it (and
// the bridge, which then runs the new `fabo`). Running containers stop;
// those with a restart policy start again.
func (h *host) restartEngine() error {
	distro := h.distro()
	if distro == "" {
		return errors.New("Fabo is not set up yet")
	}
	h.userStopped.Store(false)
	h.setEngineState("starting")
	defer h.setEngineState("")
	if _, err := h.syncEngine(distro); err != nil {
		return err
	}
	if h.wslRun(distro, "systemctl", "restart", "fabo") != nil {
		if err := h.wslRun(distro, winhost.WSLStart, "restart"); err != nil {
			return err
		}
	}
	h.mux.Reset()
	for range 100 {
		if h.engineAlive() {
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
		if !h.setupPending.Load() && !h.userStopped.Load() && h.distro() != "" && !h.engineAlive() {
			if err := h.ensureEngine(); err != nil {
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

// pushProxy tells the engine what Windows' proxy is, now and every minute.
// A proxy on Windows' own 127.0.0.1 cannot be reached from WSL without
// upstream's proxy relay (crates/kiri-win/src/relay.rs), which is not
// ported yet: the engine is told no relay ports, and uses such a proxy
// only where it can reach it.
func (h *host) pushProxy() {
	for {
		if !h.setupPending.Load() && h.distro() != "" {
			body, _ := json.Marshal(struct {
				winhost.SystemProxy
				Relay map[string]uint16 `json:"relay"`
			}{systemProxy(), map[string]uint16{}})
			// The engine keeps it in memory only: sent again every minute,
			// it survives an engine restart and follows changes.
			_ = h.engineRequest(http.MethodPut, "/fabo/proxy/system", body)
		}
		select {
		case <-h.quitting:
			return
		case <-time.After(time.Minute):
		}
	}
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
	h.engineMu.Lock()
	st.Engine = h.engineState
	h.engineMu.Unlock()
	if st.Engine == "" {
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

// updateState and updateAction are the control API's update endpoints.
func (h *host) updateState() updateView { return h.updates.state() }

// updateAction runs one update action.
func (h *host) updateAction(action string) error { return h.updates.action(action) }

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
// `fabo` command it runs for engine commands.
func (h *host) openDesktop() {
	desktop := filepath.Join(h.install, "fabo-desktop.exe")
	if _, err := os.Stat(desktop); err != nil {
		h.logf("window: %v", errNoDesktop)
		return
	}
	c := exec.Command(desktop)
	c.Dir = h.install
	c.Env = append(os.Environ(), "FABO_PIPE="+h.pipe, "FABO_LAUNCHER="+filepath.Join(h.install, "bin", "fabo.exe"))
	if err := c.Start(); err != nil {
		h.logf("window: %v", err)
		return
	}
	go c.Wait()
}
