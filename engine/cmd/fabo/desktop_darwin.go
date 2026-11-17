package main

import (
	"net/http"
)

// prepareDesktop sets the Mac up the first time the app is opened from
// Finder or the Dock. `fabo ui` from a shell changes nothing.
func (i installation) prepareDesktop(args []string) error {
	if s := newSetup(i); len(args) == 0 && s.launchedFromApp() {
		return s.firstLaunch()
	}
	return nil
}

// startDaemon starts the engine when nothing answers on the socket. Once a
// LaunchAgent runs this copy of the app, launchd is the engine's one owner:
// a second engine spawned here would race it for the socket and the state
// lock. Without the agent the launcher starts the engine itself.
func (i installation) startDaemon(client *http.Client) error {
	// Checks the engine, its guest and the store, and reports here what a
	// daemon failing under launchd could only write to its log.
	if _, _, err := i.daemonCommand(); err != nil {
		return err
	}
	if s := newSetup(i); s.agentRunsThis() && s.agentLoaded() {
		_ = s.kickstart()
		return waitAlive(client, nil, i.logPath())
	}
	return i.spawnDaemon(client)
}

// finishDesktop registers the build builder once the engine answers, when
// setup could not: the engine was not running yet.
func (i installation) finishDesktop() {
	if s := newSetup(i); !s.places().Ready() {
		_, _ = s.ensureBuilder()
	}
}
