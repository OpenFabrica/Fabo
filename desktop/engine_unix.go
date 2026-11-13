//go:build darwin || linux

package main

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"runtime"
)

func dialEngine(ctx context.Context) (net.Conn, error) {
	socket := os.Getenv("KIRI_SOCKET")
	if socket == "" && runtime.GOOS == "linux" {
		const serviceSocket = "/run/kiri/docker.sock"
		if _, err := os.Stat(serviceSocket); err == nil {
			socket = serviceSocket
		}
	}
	if socket == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		socket = filepath.Join(home, ".kiri", "run", "docker.sock")
	}
	return (&net.Dialer{}).DialContext(ctx, "unix", socket)
}
