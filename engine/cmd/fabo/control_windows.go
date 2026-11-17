package main

import (
	"context"
	"encoding/json"
	"net"
	"time"

	"github.com/OpenFabrica/Fabo/engine/internal/winhost"
)

// winhostDial connects to fabo-win's control pipe.
func winhostDial(ctx context.Context, _, _ string) (net.Conn, error) {
	return winhost.DialPipe(winhost.ControlPipe, 2*time.Second)
}

// jsonMessage is an error body's "message", or the body itself.
func jsonMessage(body []byte) string {
	var v struct{ Message string }
	if json.Unmarshal(body, &v) == nil && v.Message != "" {
		return v.Message
	}
	return string(body)
}
