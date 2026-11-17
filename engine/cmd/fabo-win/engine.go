package main

import (
	"sync"
	"sync/atomic"
)

// engineControl runs the engine's start, stop and restart in WSL one at a
// time. They are reached from the window, the `fabo` command, setup and
// the watchdog at once; two fabo-start runs overlapping would let the
// second remove the socket the first daemon just made, leaving a daemon
// nobody can reach.
type engineControl struct {
	mu sync.Mutex
	// userStopped: the window's Stop, until its Start; the watchdog leaves
	// the engine stopped meanwhile.
	userStopped atomic.Bool
	state       atomic.Value // string: "starting", "stopping" or ""

	alive   func() bool
	start   func() error
	stop    func() error
	restart func() error
	// shutdown stops all of WSL (wsl --shutdown), so new .wslconfig
	// values take effect when it starts again.
	shutdown func() error
}

// current is the start or stop in progress, "" when none.
func (e *engineControl) current() string {
	s, _ := e.state.Load().(string)
	return s
}

// ensure starts the engine unless it answers. auto (the watchdog, the end
// of setup) does nothing while the user has stopped it.
func (e *engineControl) ensure(auto bool) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if auto && e.userStopped.Load() {
		return nil
	}
	if e.alive() {
		return nil
	}
	e.state.Store("starting")
	defer e.state.Store("")
	return e.start()
}

// userStart is the window's Start.
func (e *engineControl) userStart() error {
	e.userStopped.Store(false)
	return e.ensure(false)
}

// userStop is the window's Stop.
func (e *engineControl) userStop() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.userStopped.Store(true)
	e.state.Store("stopping")
	defer e.state.Store("")
	return e.stop()
}

// userRestart is `fabo engine restart`: this install's engine, restarted.
func (e *engineControl) userRestart() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.userStopped.Store(false)
	e.state.Store("starting")
	defer e.state.Store("")
	return e.restart()
}

// userRestartWSL is Settings → Resources' restart: all of WSL stops, which
// stops every distro, not only Fabo's, and the engine starts again with
// the new .wslconfig. Under the same lock as the rest, so the watchdog
// cannot start WSL again halfway through the shutdown.
func (e *engineControl) userRestartWSL() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.userStopped.Store(false)
	e.state.Store("starting")
	defer e.state.Store("")
	if err := e.shutdown(); err != nil {
		return err
	}
	return e.start()
}
