package localengine

import (
	"context"
	"net"
	"syscall"

	"golang.org/x/sys/unix"
)

func listenBuildMirror(ctx context.Context) (net.Listener, string, error) {
	host := bridgeNetwork().Gateway.String()
	config := net.ListenConfig{Control: func(_, _ string, raw syscall.RawConn) error {
		// The bridge gets its address when the first container starts.
		var optionErr error
		if err := raw.Control(func(fd uintptr) {
			optionErr = unix.SetsockoptInt(int(fd), unix.IPPROTO_IP, unix.IP_FREEBIND, 1)
		}); err != nil {
			return err
		}
		return optionErr
	}}
	listener, err := config.Listen(ctx, "tcp4", net.JoinHostPort(host, "0"))
	return listener, host, err
}
