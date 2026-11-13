package localengine

import (
	"context"
	"net"
)

func listenBuildMirror(ctx context.Context) (net.Listener, string, error) {
	var config net.ListenConfig
	listener, err := config.Listen(ctx, "tcp4", "127.0.0.1:0")
	return listener, "host.docker.internal", err
}
