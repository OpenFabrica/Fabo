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

	// life serializes resource updates, VM release, and VM boot.
	life sync.Mutex
	// mu protects admission and the reference used for passive queries.
	mu           sync.Mutex
	current      *engineInstance
	active       bool
	lastActivity time.Time
}

var errManuallyStopped = errors.New("Fabo guest is stopped")

func (s *supervisor) init() error {
	proxy, err := localengine.OpenProxy(s.state)
	if err != nil {
		return err
	}
	s.proxy = proxy
	s.vm = s.options.vmConfiguration()
	if s.vm == nil {
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
	if !settings.Stopped {
		return s.wake(false)
	}
	engine, err := localengine.New(s.ctx, nil, s.images, s.state, localengine.VMOptions{Events: &s.events, Version: engineRelease, Builder: s.builder, Proxy: s.proxy, EngineSocket: s.socket, WindowsMounts: s.windowsProxy})
	if err != nil {
		return err
	}
	s.current = &engineInstance{engine: engine, handler: engine.Handler()}
	return nil
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

func (s *supervisor) wake(explicit bool) error {
	s.life.Lock()
	defer s.life.Unlock()
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
	guest, err := options.start(ctx, s.stateDir)
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

func (s *supervisor) close(graceful bool) error {
	s.life.Lock()
	defer s.life.Unlock()
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

func (s *supervisor) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := localengine.APIPath(r.URL.Path)
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
			CPUs      uint   `json:"cpus"`
			MemoryMiB uint64 `json:"memory_mib"`
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
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"cpus": s.vm.settings.CPUs, "memory_mib": s.vm.settings.MemoryMiB, "network": s.vm.settings.NetworkMode(),
		"running_cpus": boot.CPUs, "running_memory_mib": boot.MemoryMiB, "running_network": runningNetwork, "awake": active,
		"max_cpus": s.vm.maxCPU, "max_memory_mib": s.vm.maxMemory,
	})
}

func supervisorError(w http.ResponseWriter, status int, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"message": err.Error()})
}
