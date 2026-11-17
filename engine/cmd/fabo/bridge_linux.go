package main

import (
	"bufio"
	"errors"
	"flag"
	"net"
	"os"

	"github.com/OpenFabrica/Fabo/engine/internal/wslbridge"
)

// wslSocket is where contrib/wsl runs the engine, as root with
// HOME=/var/lib/fabo-daemon; fabo-win names it with --socket too.
const wslSocket = "/var/lib/fabo-daemon/.fabo/run/docker.sock"

// bridgeCommand is `fabo bridge`, the WSL end of fabo-win.exe: Docker API
// connections from Windows arrive multiplexed on stdin, each relayed to
// the engine's Unix socket, and the replies leave on stdout. It ends when
// fabo-win closes stdin.
func bridgeCommand(args []string) error {
	flags := flag.NewFlagSet("fabo bridge", flag.ContinueOnError)
	socket := flags.String("socket", "", "the engine's Unix socket (default: this installation's, or Fabo Linux's)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("usage: fabo bridge [--socket PATH]")
	}
	path := *socket
	if path == "" {
		if _, err := os.Stat(wslSocket); err == nil {
			path = wslSocket
		} else if in, err := locate(); err == nil {
			path = in.socket()
		} else {
			path = wslSocket
		}
	}
	addr := &net.UnixAddr{Name: path, Net: "unix"}
	dial := func() (wslbridge.Conn, error) {
		c, err := net.DialUnix("unix", nil, addr)
		if err != nil {
			return nil, err
		}
		return c, nil
	}
	in := bufio.NewReaderSize(os.Stdin, 64<<10)
	return wslbridge.Serve(in, os.Stdout, dial)
}
