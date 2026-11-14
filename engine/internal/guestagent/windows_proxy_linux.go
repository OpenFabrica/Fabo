package guestagent

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/OpenFabrica/Fabo/engine/internal/guestwire"
)

func windowsProxyPorts(options []string, socket string) ([]uint16, error) {
	var ports []uint16
	if len(options) > 2 || (len(options) != 0 && socket == "") {
		return nil, fmt.Errorf("unsupported Go guest run options")
	}
	for _, option := range options {
		raw, ok := strings.CutPrefix(option, guestwire.WindowsProxyOption)
		port, err := strconv.ParseUint(raw, 10, 16)
		if !ok || err != nil || port == 0 || slices.Contains(ports, uint16(port)) {
			return nil, fmt.Errorf("invalid Windows proxy option")
		}
		ports = append(ports, uint16(port))
	}
	return ports, nil
}

func (n *containerNetwork) startWindowsProxy(socket string, ports []uint16) error {
	if len(ports) == 0 {
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	var listeners []*net.TCPListener
	n.closeProxy = func() {
		cancel()
		for _, l := range listeners {
			_ = l.Close()
		}
	}
	err := inNetworkNamespace(n.ns, func() error {
		for _, port := range ports {
			l, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.ParseIP(guestwire.WindowsProxyAddress), Port: int(port)})
			if err != nil {
				return err
			}
			listeners = append(listeners, l)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("listen for Windows proxy inside the container: %w", err)
	}
	for i, l := range listeners {
		go n.acceptWindowsProxy(ctx, l, socket, ports[i])
	}
	return nil
}

func (n *containerNetwork) acceptWindowsProxy(ctx context.Context, l *net.TCPListener, socket string, port uint16) {
	for {
		conn, err := l.AcceptTCP()
		if err != nil {
			return
		}
		if !n.canUseWindowsProxy() {
			_ = conn.Close()
			continue
		}
		go forwardWindowsProxy(ctx, conn, socket, port)
	}
}

func (n *containerNetwork) canUseWindowsProxy() bool {
	n.mu.RLock()
	defer n.mu.RUnlock()
	for _, ep := range n.endpoints {
		if !ep.bridge.internal {
			return true
		}
	}
	return false
}

func forwardWindowsProxy(ctx context.Context, conn *net.TCPConn, socket string, port uint16) {
	defer conn.Close()
	upstream, err := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "unix", socket)
	if err != nil {
		return
	}
	defer upstream.Close()
	stop := context.AfterFunc(ctx, func() {
		_ = upstream.Close()
		_ = conn.Close()
	})
	defer stop()
	_ = upstream.SetDeadline(time.Now().Add(6 * time.Second))
	if _, err := fmt.Fprintf(upstream, "CONNECT %s%d HTTP/1.1\r\nHost: localhost\r\n\r\n", guestwire.WindowsProxyPath, port); err != nil {
		return
	}
	reader := bufio.NewReader(upstream)
	response, err := http.ReadResponse(reader, &http.Request{Method: http.MethodConnect})
	if err != nil || response.StatusCode != http.StatusOK {
		return
	}
	_ = upstream.SetDeadline(time.Time{})
	guestwire.BridgeTCP(conn, bufferedProxyConn{TCPStream: upstream.(guestwire.TCPStream), reader: reader})
}

type bufferedProxyConn struct {
	guestwire.TCPStream
	reader *bufio.Reader
}

func (c bufferedProxyConn) Read(p []byte) (int, error) { return c.reader.Read(p) }
