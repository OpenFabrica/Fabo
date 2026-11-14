package main

import (
	"errors"
	"net/http"
)

// On Linux the package manager installs the engine and systemd runs it, so
// there is nothing for the launcher to set up.

func setupCommand([]string) error {
	return errors.New("fabo setup is for macOS; on Linux the package sets the engine up")
}

func buildWithCommand([]string) error {
	return errors.New("fabo build-with is not available on Linux yet")
}

// domainsCommand has nothing to switch: Linux has *.fabo.localhost only.
// machineNames is the suffix local addresses carry on Linux, which needs
// no resolver rule, and no trusted certificate.
func machineNames(installation) (string, bool) { return "fabo.localhost", false }

func domainsCommand([]string) error {
	return errors.New("*.fabo.local names and HTTPS are not available on Linux yet")
}

func (i installation) prepareDesktop([]string) error { return nil }

// startDaemon never starts the engine: it is a system service there.
func (i installation) startDaemon(*http.Client) error {
	return errors.New("the Fabo engine is not running: sudo systemctl start fabo")
}

func (i installation) finishDesktop() {}

func reportFailure(error) {}

// contextCommand is macOS-only: on Linux plain docker reaches Fabo through
// the standard socket, with no context to switch.
func contextCommand([]string) error {
	return errors.New("docker contexts are switched by fabo only on macOS")
}
