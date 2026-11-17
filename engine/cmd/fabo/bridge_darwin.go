package main

import "errors"

// bridgeCommand is the WSL end of the Windows host, which a Mac never runs.
func bridgeCommand([]string) error {
	return errors.New("fabo bridge runs in WSL, for Fabo on Windows")
}
