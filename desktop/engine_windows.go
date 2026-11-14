package main

import (
	"context"
	"net"
	"os"
	"strings"

	"github.com/Microsoft/go-winio"
)

// enginePipe is the named pipe the engine listens on.
func enginePipe() string {
	if pipe := os.Getenv("KIRI_PIPE"); pipe != "" {
		return pipe
	}
	return `\\.\pipe\kiri`
}

func dialEngine(ctx context.Context) (net.Conn, error) {
	return winio.DialPipeContext(ctx, enginePipe())
}

// engineHost is the engine's address as DOCKER_HOST spells it, for the
// docker tools the window runs.
func engineHost() string {
	return "npipe://" + strings.ReplaceAll(enginePipe(), `\\`, "/")
}
