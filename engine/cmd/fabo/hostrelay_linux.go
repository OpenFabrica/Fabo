package main

import (
	"errors"
	"os"

	"github.com/OpenFabrica/Fabo/engine/internal/hostrelay"
)

// hostrelayCommand is `fabo hostrelay`, the WSL end of fabo-win's proxy
// relay: it listens on the distro's loopback for each proxy port on
// Windows' loopback that fabo-win asks for, and carries the connections
// over stdin and stdout. Only frames go to stdout. It ends, and its
// listeners with it, when fabo-win closes stdin.
func hostrelayCommand(args []string) error {
	if len(args) != 0 {
		return errors.New("usage: fabo hostrelay")
	}
	return hostrelay.ListenSide(os.Stdin, os.Stdout)
}
