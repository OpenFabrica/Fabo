//go:build darwin || linux

package main

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"runtime"
)

// engineSocket is where the engine listens: FABO_SOCKET, else the system
// service's socket on Linux, else ~/.fabo/run/docker.sock.
func engineSocket() (string, error) {
	socket := os.Getenv("FABO_SOCKET")
	if socket == "" && runtime.GOOS == "linux" {
		const serviceSocket = "/run/fabo/docker.sock"
		if _, err := os.Stat(serviceSocket); err == nil {
			socket = serviceSocket
		}
	}
	if socket == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		socket = filepath.Join(home, ".fabo", "run", "docker.sock")
	}
	return socket, nil
}

func dialEngine(ctx context.Context) (net.Conn, error) {
	socket, err := engineSocket()
	if err != nil {
		return nil, err
	}
	return (&net.Dialer{}).DialContext(ctx, "unix", socket)
}

// engineHost is the engine's address as DOCKER_HOST spells it, for the
// docker tools the window runs.
func engineHost() string {
	socket, err := engineSocket()
	if err != nil {
		return ""
	}
	return "unix://" + socket
}

// dialHost is nil: only Windows has a host process (fabo-win).
var dialHost func(context.Context) (net.Conn, error)

// singleInstance is false: the macOS app and Linux packages open the
// window once themselves.
const singleInstance = false
