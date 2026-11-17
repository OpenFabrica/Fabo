package main

import "errors"

// hostrelayCommand is the WSL end of fabo-win's proxy relay, which a Mac
// never runs.
func hostrelayCommand([]string) error {
	return errors.New("fabo hostrelay runs in WSL, for Fabo on Windows")
}
