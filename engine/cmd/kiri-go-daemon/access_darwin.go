//go:build cgo

package main

import (
	"context"
	"net"
)

func socketAccessGroup() string { return "" }

func configureSocket(_ context.Context, listener *net.UnixListener, _ string) (net.Listener, error) {
	return listener, nil
}
