//go:build linux || (darwin && cgo)

package main

import (
	"errors"
	"net"
	"os"
	"syscall"
	"time"
)

// listenSocket may remove a stale socket after the caller locks its private
// state. An active socket or another kind of filesystem entry stays intact.
func listenSocket(path string) (*net.UnixListener, error) {
	addr := &net.UnixAddr{Name: path, Net: "unix"}
	listener, err := net.ListenUnix("unix", addr)
	if !errors.Is(err, syscall.EADDRINUSE) {
		return listener, err
	}
	info, statErr := os.Lstat(path)
	if statErr != nil || info.Mode()&os.ModeSocket == 0 {
		return nil, err
	}
	conn, dialErr := net.DialTimeout("unix", path, 250*time.Millisecond)
	if dialErr == nil {
		conn.Close()
		return nil, err
	}
	if !errors.Is(dialErr, syscall.ECONNREFUSED) {
		return nil, err
	}
	if err := os.Remove(path); err != nil {
		return nil, err
	}
	return net.ListenUnix("unix", addr)
}
