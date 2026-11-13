package main

import (
	"errors"
	"net/http"
)

// On Linux the package manager installs the engine and systemd runs it, so
// there is nothing for the launcher to set up.

func setupCommand([]string) error {
	return errors.New("kiri setup is for macOS; on Linux the package sets the engine up")
}

func buildWithCommand([]string) error {
	return errors.New("kiri build-with is not available on Linux yet")
}

// domainsCommand has nothing to switch: Linux has *.kiri.localhost only.
func domainsCommand([]string) error {
	return errors.New("*.kiri.local names and HTTPS are not available on Linux yet")
}

func (i installation) prepareDesktop([]string) error { return nil }

// startDaemon never starts the engine: it is a system service there.
func (i installation) startDaemon(*http.Client) error {
	return errors.New("the Kiri engine is not running: sudo systemctl start kiri")
}

func (i installation) finishDesktop() {}

func reportFailure(error) {}
