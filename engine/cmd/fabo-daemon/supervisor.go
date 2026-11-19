//go:build linux || (darwin && cgo)

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/OpenFabrica/Fabo/engine/internal/guestwire"
	"github.com/OpenFabrica/Fabo/engine/internal/localengine"
)

type vmConfiguration struct {
	minCPU, maxCPU       uint
	minMemory, maxMemory uint64
	settings             localengine.VMSettings
}

func (v *vmConfiguration) validate(settings localengine.VMSettings) error {
	if settings.CPUs < v.minCPU || settings.CPUs > v.maxCPU {
		return fmt.Errorf("cpus must be between %d and %d", v.minCPU, v.maxCPU)
	}
	if settings.MemoryMiB < v.minMemory || settings.MemoryMiB > v.maxMemory {
		return fmt.Errorf("memory_mib must be between %d and %d", v.minMemory, v.maxMemory)
	}
	if !localengine.ValidVMNetwork(settings.Network) {
		return fmt.Errorf("network must be %q or %q", localengine.VMNetworkUser, localengine.VMNetworkNAT)
	}
	return nil
}

type engineInstance struct {
	engine   *localengine.Engine
	handler  http.Handler
	guest    guestConnection
	usb      *usbSessions
	ctx      context.Context
	cancel   context.CancelFunc
	requests sync.WaitGroup
	inFlight int
	boot     localengine.VMSettings
}

type supervisor struct {
	windowsProxy bool
	socket       string
	ctx          context.Context
	options      guestOptions
	state        *localengine.State
	stateDir     string
	builder      string
	images       map[string]localengine.Image
	vm           *vmConfiguration
	events       localengine.EventBus
	proxy        *localengine.Proxy
	domains      *localengine.Domains
	buildMirror  *localengine.BuildMirror
	// shares hosts public share links; nil when the launcher passed no
	// -share-config.
	shares *localengine.PublicShares
	// wakeForShare boots the VM for a share link; tests replace it.
	wakeForShare func() error
	// startGuest boots the guest with the given options; tests replace
	// it. nil runs options.start.
	startGuest func(ctx context.Context, options guestOptions) (guestConnection, error)

	// life serializes resource updates, VM release, and VM boot.
	life sync.Mutex
	// mu protects admission and the reference used for passive queries.
	mu           sync.Mutex
	current      *engineInstance
	active       bool
	lastActivity time.Time
	// bootErr is why the VM last failed to boot; nil once it boots or the
	// user stops it.
	bootErr error
	// cancelBootRetry stops the background boot retry; nil when none runs.
	cancelBootRetry context.CancelFunc

	// closing is set under life once the daemon shuts down: no boot after.
	closing bool
}

var (
	errManuallyStopped = errors.New("Fabo guest is stopped")
	errDaemonClosing   = errors.New("the daemon is shutting down")
)

// The background boot retry waits bootRetryFirst, then twice as long each
// time, up to bootRetryMax; tests shorten them.
var (
	bootRetryFirst = 10 * time.Second
	bootRetryMax   = 5 * time.Minute
)

// init opens the proxy, reads the VM settings and brings up the first engine.
func (s *supervisor) init() error {
	proxy, err := localengine.OpenProxy(s.state)
	if err != nil {
		return err
	}
	s.proxy = proxy
	s.vm = s.options.vmConfiguration()
	if s.vm == nil {
		// The Linux agent: the daemon lives as long as the agent's session
		// (run), so there is nothing to serve without it.
		return s.wake(false)
	}
	settings, err := s.state.LoadVMSettings(s.vm.settings)
	if err != nil {
		return err
	}
	if err := s.vm.validate(settings); err != nil {
		return err
	}
	s.vm.settings = settings
	return s.startVM()
}

// startVM boots the VM unless the user stopped it. When it does not boot,
// the daemon serves without a guest, as when stopped, and tries again in the
// background: exiting only had launchd start a daemon that failed the same
// way every 10 s, with no socket for the window to read the reason from.
func (s *supervisor) startVM() error {
	var bootErr error
	if !s.vm.settings.Stopped {
		if bootErr = s.wake(false); bootErr == nil {
			return nil
		}
		log.Printf("the Fabo VM did not start: %v; serving without it and trying again in %v", bootErr, bootRetryFirst)
	}
	engine, err := localengine.New(s.ctx, nil, s.images, s.state, localengine.VMOptions{Events: &s.events, Version: engineRelease, Builder: s.builder, Proxy: s.proxy, EngineSocket: s.socket, WindowsMounts: s.windowsProxy})
	if err != nil {
		return errors.Join(bootErr, err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.current = &engineInstance{engine: engine, handler: engine.Handler()}
	if bootErr != nil {
		s.restartBootRetryLocked()
	}
	return nil
}

// restartBootRetryLocked starts the background boot retry from its first
// delay, replacing one already running. The caller holds mu.
func (s *supervisor) restartBootRetryLocked() {
	s.stopBootRetryLocked()
	ctx, cancel := context.WithCancel(s.ctx)
	s.cancelBootRetry = cancel
	// Read the delays once, so a running retry keeps its own schedule.
	go s.retryBoot(ctx, bootRetryFirst, bootRetryMax)
}

// stopBootRetryLocked cancels the background boot retry, if any. The caller
// holds mu.
func (s *supervisor) stopBootRetryLocked() {
	if s.cancelBootRetry != nil {
		s.cancelBootRetry()
		s.cancelBootRetry = nil
	}
}

// nextBootDelay doubles a retry delay up to most.
func nextBootDelay(delay, most time.Duration) time.Duration {
	return min(delay*2, most)
}

// retryBoot boots the VM after a growing delay until it is up, the user
// stops it, the retry is cancelled, or the daemon shuts down. It holds no
// lock while waiting; wake itself checks the stop state under life.
func (s *supervisor) retryBoot(ctx context.Context, delay, most time.Duration) {
	for {
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		err := s.wake(false)
		if err == nil || errors.Is(err, errManuallyStopped) || errors.Is(err, errDaemonClosing) || ctx.Err() != nil {
			return
		}
		delay = nextBootDelay(delay, most)
		log.Printf("the Fabo VM did not start: %v; trying again in %v", err, delay)
	}
}

func (s *supervisor) isActiveLocked() bool {
	if !s.active {
		return false
	}
	select {
	case <-s.current.engine.Done():
		return false
	default:
		return true
	}
}

func (s *supervisor) mirrorEngine() *localengine.Engine {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.current == nil {
		return nil
	}
	return s.current.engine
}

// wake boots the VM unless it runs already, or, unless explicit, the user
// stopped it. It records the outcome as bootErr for /fabo/resources, and a
// boot ends the background retry.
func (s *supervisor) wake(explicit bool) error {
	s.life.Lock()
	defer s.life.Unlock()
	err := s.wakeLocked(explicit)
	if !errors.Is(err, errManuallyStopped) && !errors.Is(err, errDaemonClosing) {
		s.mu.Lock()
		s.bootErr = err
		if err == nil {
			s.stopBootRetryLocked()
		}
		s.mu.Unlock()
	}
	return err
}

// wakeLocked is wake's body; the caller holds life.
func (s *supervisor) wakeLocked(explicit bool) error {
	if s.closing {
		return errDaemonClosing
	}
	if !explicit && s.vm != nil && s.vm.settings.Stopped {
		return errManuallyStopped
	}
	s.mu.Lock()
	alreadyRunning := s.isActiveLocked()
	s.mu.Unlock()
	if alreadyRunning {
		return nil
	}
	if err := s.retire(false); err != nil {
		return fmt.Errorf("release previous guest: %w", err)
	}

	settings := localengine.VMSettings{}
	options := s.options
	if s.vm != nil {
		settings = s.vm.settings
		options = options.withVMSettings(settings)
	}
	ctx, cancel := context.WithCancel(s.ctx)
	start := s.startGuest
	if start == nil {
		start = func(ctx context.Context, options guestOptions) (guestConnection, error) {
			return options.start(ctx, s.stateDir)
		}
	}
	guest, err := start(ctx, options)
	if err != nil {
		cancel()
		return err
	}
	engine, err := localengine.New(ctx, guest.conn, s.images, s.state, localengine.VMOptions{
		Shares: guest.shares, Disk: guest.disk, HostCgroups: guest.cgroups, Relay: guest.relay, Archive: guest.archive, CPUs: settings.CPUs, MemoryMiB: settings.MemoryMiB, Events: &s.events, Version: engineRelease, Builder: s.builder, Proxy: s.proxy, Domains: s.domains, EngineSocket: s.socket, WindowsMounts: s.windowsProxy,
		Rosetta: guest.rosetta, BuildMirror: s.buildMirror,
	})
	if err != nil {
		cancel()
		return errors.Join(err, guest.close())
	}
	if s.vm != nil && settings.Stopped {
		settings.Stopped = false
		if err := s.state.SaveVMSettings(settings); err != nil {
			engine.Close()
			cancel()
			return errors.Join(err, guest.close())
		}
		s.mu.Lock()
		s.vm.settings = settings
		s.mu.Unlock()
	}
	s.mu.Lock()
	instance := &engineInstance{engine: engine, handler: engine.Handler(), guest: guest, ctx: ctx, cancel: cancel, boot: settings}
	if s.vm != nil && guest.relay != nil {
		instance.usb = newUSBSessions(guest.relay)
	}
	s.current = instance
	s.active = true
	s.lastActivity = time.Now()
	s.mu.Unlock()
	return nil
}

// sleep shuts the VM down; manualStop is the user's Stop, which is saved,
// ends the background boot retry and clears the last boot failure.
func (s *supervisor) sleep(manualStop bool) error {
	s.life.Lock()
	defer s.life.Unlock()
	if manualStop && !s.vm.settings.Stopped {
		next := s.vm.settings
		next.Stopped = true
		if err := s.state.SaveVMSettings(next); err != nil {
			return err
		}
		s.mu.Lock()
		s.vm.settings = next
		s.mu.Unlock()
	}
	if manualStop {
		s.mu.Lock()
		s.stopBootRetryLocked()
		s.bootErr = nil
		s.mu.Unlock()
	}
	return s.retire(true)
}

func (s *supervisor) sleepIfIdle(now time.Time, idle time.Duration) error {
	s.life.Lock()
	defer s.life.Unlock()
	s.mu.Lock()
	quietFor := now.Sub(s.lastActivity)
	if !s.isActiveLocked() || s.current.inFlight != 0 || quietFor < idle || !s.current.engine.PrepareIdleShutdown() {
		s.mu.Unlock()
		return nil
	}
	old := s.current
	s.active = false
	s.mu.Unlock()
	return old.shutdown(s.ctx, true)
}

func idleTimeout(raw string, fallback time.Duration) time.Duration {
	seconds, err := strconv.ParseUint(raw, 10, 64)
	if err != nil || seconds > uint64(math.MaxInt64/int64(time.Second)) {
		return fallback
	}
	return time.Duration(seconds) * time.Second
}

func (s *supervisor) monitorIdle(ctx context.Context, idle time.Duration) {
	interval := 5 * time.Second
	if s.vm != nil && idle > 0 {
		interval = min(interval, idle)
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			s.mu.Lock()
			if !s.isActiveLocked() {
				s.mu.Unlock()
				continue
			}
			current := s.current
			vmIdle := s.vm != nil && idle > 0 && current.inFlight == 0 && now.Sub(s.lastActivity) >= idle
			s.mu.Unlock()
			pause, cancel := context.WithTimeout(ctx, 5*time.Second)
			err := current.engine.PauseIdleBuilders(pause, now, vmIdle)
			cancel()
			if err != nil && ctx.Err() == nil {
				log.Printf("pause idle BuildKit: %v", err)
			}
			if !vmIdle {
				continue
			}
			if err := s.sleepIfIdle(now, idle); err != nil && ctx.Err() == nil {
				log.Printf("put Fabo VM to sleep: %v", err)
			}
		}
	}
}

// retire is called under life. Passive routes keep using the final snapshot
// while the guest stops. No new write can enter the retiring instance.
func (s *supervisor) retire(graceful bool) error {
	s.mu.Lock()
	if !s.active {
		s.mu.Unlock()
		return nil
	}
	old := s.current
	s.active = false
	s.mu.Unlock()
	return old.shutdown(s.ctx, graceful)
}

func (old *engineInstance) shutdown(parent context.Context, graceful bool) error {
	old.usb.close()
	var err error
	if graceful {
		ctx, cancel := context.WithTimeout(parent, 25*time.Second)
		err = old.engine.StopContainers(ctx)
		cancel()
	}
	old.engine.Close()
	old.cancel()
	guestErr := old.guest.close()
	old.requests.Wait()
	return errors.Join(err, guestErr)
}

// close shuts the guest down for good: no boot, retried or not, follows.
func (s *supervisor) close(graceful bool) error {
	s.life.Lock()
	defer s.life.Unlock()
	s.closing = true
	s.mu.Lock()
	s.stopBootRetryLocked()
	s.mu.Unlock()
	return s.retire(graceful)
}

func (s *supervisor) dialDomain(ctx context.Context, host string) (net.Conn, error) {
	s.mu.Lock()
	var engine *localengine.Engine
	if s.isActiveLocked() {
		engine = s.current.engine
	}
	s.mu.Unlock()
	return engine.DialDomain(ctx, host)
}

// shareWakeTimeout bounds waiting for the VM to boot for a share link,
// separately from the dial that follows.
const shareWakeTimeout = 60 * time.Second

// shareEngine returns the running engine for a public share link, waking
// the VM when it sleeps. A visitor counts as activity, so the VM does not
// go idle under a link that is being used.
func (s *supervisor) shareEngine(ctx context.Context) (engine *localengine.Engine, woke bool, err error) {
	s.mu.Lock()
	if s.isActiveLocked() {
		engine := s.current.engine
		s.lastActivity = time.Now()
		s.mu.Unlock()
		return engine, false, nil
	}
	stopped := s.vm == nil || s.vm.settings.Stopped
	s.mu.Unlock()
	if stopped {
		return nil, false, errManuallyStopped
	}
	wake := s.wakeForShare
	if wake == nil {
		wake = func() error { return s.wake(false) }
	}
	// wake takes life itself and is not cancellable; a slow boot keeps
	// going in the background while this visitor gives up.
	booted := make(chan error, 1)
	go func() { booted <- wake() }()
	wait, cancel := context.WithTimeout(ctx, shareWakeTimeout)
	defer cancel()
	select {
	case err := <-booted:
		if err != nil {
			return nil, false, err
		}
	case <-wait.Done():
		return nil, false, fmt.Errorf("the Fabo VM didn't start in time: %w", wait.Err())
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.isActiveLocked() {
		return nil, false, errManuallyStopped
	}
	s.lastActivity = time.Now()
	return s.current.engine, true, nil
}

// shareRestartWait is how long a visitor that woke the VM waits for the
// container to come back: the VM sleeps only with no container running, so
// a shared container returns through its restart policy just after boot.
var shareRestartWait = 10 * time.Second

// dialShare connects a share link's visitor to a container's HTTP service.
func (s *supervisor) dialShare(ctx context.Context, name string) (net.Conn, error) {
	engine, woke, err := s.shareEngine(ctx)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(shareRestartWait)
	for {
		dial, cancel := context.WithTimeout(ctx, 10*time.Second)
		conn, err := engine.DialContainer(dial, name)
		cancel()
		if err == nil || !woke || time.Now().After(deadline) {
			return conn, err
		}
		select {
		case <-ctx.Done():
			return nil, err
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// resolveShareContainer checks a container can be shared and returns its name.
func (s *supervisor) resolveShareContainer(ctx context.Context, name string) (string, error) {
	engine, _, err := s.shareEngine(ctx)
	if err != nil {
		return "", err
	}
	return engine.ShareableContainer(name)
}

func (s *supervisor) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := localengine.APIPath(r.URL.Path)
	// Share links don't need the guest: they are served before the wake
	// logic below, and wake the VM only when a visitor reaches a container.
	// The relay's peers are read without waking the VM: a connection to
	// look up exists only while it runs.
	if path == "/fabo/debug/peers" {
		s.mu.Lock()
		var engine *localengine.Engine
		if s.isActiveLocked() {
			engine = s.current.engine
		}
		s.mu.Unlock()
		servePeers(w, r, hostPeers, engine)
		return
	}
	if path == "/fabo/share" || strings.HasPrefix(path, "/fabo/share/") {
		if s.shares == nil {
			supervisorError(w, http.StatusNotFound, errors.New("public share links are not set up for this daemon"))
			return
		}
		s.shares.ServeHTTP(w, r)
		return
	}
	if s.windowsProxy {
		switch {
		case path == "/fabo/proxy/system":
			s.proxy.UpdateWindows(w, r)
			return
		case strings.HasPrefix(path, guestwire.WindowsProxyPath):
			s.proxy.ConnectWindows(w, r, path)
			return
		}
	}
	switch path {
	case "/fabo/domains":
		s.domains.ServeHTTP(w, r)
		return
	case "/fabo/proxy":
		s.proxy.ServeHTTP(w, r)
		return
	case "/fabo/wake", "/fabo/sleep", "/fabo/stop", "/fabo/resources":
		s.manageVM(w, r, path)
		return
	}
	var device usbDevice
	usbAttach := path == usbPath && r.Method == http.MethodPost
	if usbAttach {
		var err error
		device, err = parseUSBDevice(r)
		if err != nil {
			supervisorError(w, http.StatusBadRequest, err)
			return
		}
	}
	usbLocation, usbDetach := strings.CutPrefix(path, usbPath+"/")
	usbDetach = usbDetach && r.Method == http.MethodDelete
	usbList := path == usbPath && (r.Method == http.MethodGet || r.Method == http.MethodHead)
	needsGuest := requiresGuest(r.Method, path)
	s.mu.Lock()
	if path == "/system/df" && r.Method == http.MethodGet && (s.vm == nil || !s.vm.settings.Stopped) {
		needsGuest = true
	}
	if needsGuest && s.vm != nil && s.vm.settings.Stopped {
		s.mu.Unlock()
		supervisorError(w, http.StatusServiceUnavailable, errManuallyStopped)
		return
	}
	if needsGuest && !s.isActiveLocked() {
		s.mu.Unlock()
		if s.vm == nil {
			supervisorError(w, http.StatusServiceUnavailable, errManuallyStopped)
			return
		}
		if err := s.wake(false); err != nil {
			supervisorError(w, http.StatusServiceUnavailable, err)
			return
		}
		s.mu.Lock()
		if !s.isActiveLocked() || s.vm.settings.Stopped {
			s.mu.Unlock()
			supervisorError(w, http.StatusServiceUnavailable, errManuallyStopped)
			return
		}
	}
	current, active := s.current, s.isActiveLocked()
	stopped := s.vm != nil && s.vm.settings.Stopped
	_, background := r.Header[http.CanonicalHeaderKey("X-Fabo-Background")]
	foreground := !background && path != "/fabo/images/scan" && !usbList && !usbDetach
	if active && foreground {
		s.lastActivity = time.Now()
	}
	if needsGuest {
		current.requests.Add(1)
		current.inFlight++
	}
	s.mu.Unlock()
	if needsGuest {
		defer func() {
			s.mu.Lock()
			current.inFlight--
			if current == s.current && foreground {
				s.lastActivity = time.Now()
			}
			s.mu.Unlock()
			current.requests.Done()
		}()
		ctx, cancel := context.WithCancel(r.Context())
		stop := context.AfterFunc(current.ctx, cancel)
		defer stop()
		defer cancel()
		r = r.WithContext(ctx)
	}
	if path == "/info" && (r.Method == http.MethodGet || r.Method == http.MethodHead) {
		current.engine.WriteInfo(w, active, stopped)
		return
	}
	switch {
	case usbAttach:
		current.usb.attach(w, r, device)
		return
	case usbList:
		current.usb.list(w)
		return
	case usbDetach:
		current.usb.detach(w, r, usbLocation)
		return
	}
	current.handler.ServeHTTP(w, r)
}

func requiresGuest(method, path string) bool {
	if path == "/fabo/builder" {
		return false
	}
	if method == http.MethodPost && path == "/fabo/images/scan" {
		return false
	}
	if method == http.MethodDelete && strings.HasPrefix(path, usbPath+"/") {
		return false
	}
	if method != http.MethodGet && method != http.MethodHead {
		return true
	}
	if resource, ok := strings.CutPrefix(path, "/containers/"); ok {
		_, action, _ := strings.Cut(resource, "/")
		return action == "archive" || action == "changes" || action == "export"
	}
	return false
}

func (s *supervisor) manageVM(w http.ResponseWriter, r *http.Request, path string) {
	if s.vm == nil {
		if path == "/fabo/resources" && r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"cpus": 0, "memory_mib": 0, "awake": false})
			return
		}
		supervisorError(w, http.StatusNotImplemented, errors.New("this host runs a Linux agent without VM controls"))
		return
	}
	if path == "/fabo/resources" {
		s.resources(w, r)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var err error
	if path == "/fabo/wake" {
		err = s.wake(true)
		// Start that fails while the VM is meant to run (not stopped)
		// starts the background retry over from its first delay.
		s.mu.Lock()
		if err != nil && !errors.Is(err, errDaemonClosing) && !s.vm.settings.Stopped {
			s.restartBootRetryLocked()
		}
		s.mu.Unlock()
	} else {
		err = s.sleep(path == "/fabo/stop")
	}
	if err != nil {
		supervisorError(w, http.StatusInternalServerError, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *supervisor) resources(w http.ResponseWriter, r *http.Request) {
	s.life.Lock()
	defer s.life.Unlock()
	switch r.Method {
	case http.MethodGet:
	case http.MethodPost:
		var input struct {
			CPUs      uint    `json:"cpus"`
			MemoryMiB uint64  `json:"memory_mib"`
			Network   *string `json:"network"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&input); err != nil {
			supervisorError(w, http.StatusBadRequest, err)
			return
		}
		next := localengine.VMSettings{CPUs: input.CPUs, MemoryMiB: input.MemoryMiB, Network: s.vm.settings.Network, Stopped: s.vm.settings.Stopped}
		// Clients that predate the network mode post only the size; keep the saved mode for them.
		if input.Network != nil {
			next.Network = *input.Network
		}
		if err := s.vm.validate(next); err != nil {
			supervisorError(w, http.StatusBadRequest, err)
			return
		}
		if err := s.state.SaveVMSettings(next); err != nil {
			supervisorError(w, http.StatusInternalServerError, err)
			return
		}
		s.mu.Lock()
		s.vm.settings = next
		s.mu.Unlock()
	default:
		w.Header().Set("Allow", "GET, POST")
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	s.mu.Lock()
	active := s.isActiveLocked()
	boot, runningNetwork := localengine.VMSettings{}, ""
	if active {
		boot, runningNetwork = s.current.boot, s.current.boot.NetworkMode()
	}
	bootErr := s.bootErr
	s.mu.Unlock()
	out := map[string]any{
		"cpus": s.vm.settings.CPUs, "memory_mib": s.vm.settings.MemoryMiB, "network": s.vm.settings.NetworkMode(),
		"running_cpus": boot.CPUs, "running_memory_mib": boot.MemoryMiB, "running_network": runningNetwork, "awake": active,
		"max_cpus": s.vm.maxCPU, "max_memory_mib": s.vm.maxMemory,
	}
	// Only while the last boot failed, so older clients see no change.
	if bootErr != nil {
		out["boot_error"] = bootErr.Error()
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

func supervisorError(w http.ResponseWriter, status int, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"message": err.Error()})
}
