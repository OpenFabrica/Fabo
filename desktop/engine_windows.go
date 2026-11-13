package main

import (
	"context"
	"net"
	"os"

	"github.com/Microsoft/go-winio"
)

func dialEngine(ctx context.Context) (net.Conn, error) {
	pipe := os.Getenv("KIRI_PIPE")
	if pipe == "" {
		pipe = `\\.\pipe\kiri`
	}
	return winio.DialPipeContext(ctx, pipe)
}
