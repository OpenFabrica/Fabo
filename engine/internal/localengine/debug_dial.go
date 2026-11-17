//go:build linux || darwin

package localengine

// POST /fabo/debug/dial connects a client to a port inside a container,
// published or not: the desktop's Debug page uses it to reach the
// container network, which the Mac cannot route to. The connection is
// handed over by upgrading the request, like attach. TCP is a raw stream;
// UDP datagrams travel framed by guestwire's datagram frames, in both
// directions.

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/OpenFabrica/Fabo/engine/internal/guestwire"
)

// debugMark tells a client that the debug endpoints answered, so a 404
// from them is about the container, not a missing endpoint.
const debugMark = "X-Fabo-Debug"

// debugDialHTTP is POST /fabo/debug/dial?container=&port=&proto=.
func (e *Engine) debugDialHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set(debugMark, "1")
	query := r.URL.Query()
	name := strings.TrimPrefix(query.Get("container"), "/")
	network := query.Get("proto")
	if network == "" {
		network = "tcp"
	}
	port, err := strconv.ParseUint(query.Get("port"), 10, 16)
	if name == "" || err != nil || port == 0 || (network != "tcp" && network != "udp") {
		writeError(w, fmt.Errorf("%w: container, a port from 1 to 65535 and proto tcp or udp are required", errInvalid))
		return
	}
	upstream, err := e.dialContainerPort(r.Context(), name, uint16(port), network)
	if err != nil {
		writeError(w, err)
		return
	}
	defer upstream.Close()
	conn, rw, err := http.NewResponseController(w).Hijack()
	if err != nil {
		writeError(w, err)
		return
	}
	defer conn.Close()
	_ = conn.SetWriteDeadline(time.Now().Add(15 * time.Second))
	if _, err := rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n" + debugMark + ": 1\r\n\r\n"); err != nil {
		return
	}
	if err := rw.Flush(); err != nil {
		return
	}
	_ = conn.SetWriteDeadline(time.Time{})
	// Closing both ends when the engine stops ends the copies below.
	stop := context.AfterFunc(r.Context(), func() {
		_ = conn.Close()
		_ = upstream.Close()
	})
	defer stop()
	client := &bufferedConn{Conn: conn, reader: rw.Reader}
	if network == "udp" && e.guestRelay == nil {
		bridgeDatagrams(client, upstream)
		return
	}
	// TCP, and UDP through the guest relay whose frames are the client's.
	pipe(client, upstream)
}

// dialContainerPort opens a connection to a running container's port on
// its bridge address, through the guest relay in a VM.
func (e *Engine) dialContainerPort(ctx context.Context, name string, port uint16, network string) (net.Conn, error) {
	target, ok := e.domainTarget(name)
	if !ok {
		e.mu.Lock()
		c, err := e.find(name)
		e.mu.Unlock()
		if err != nil {
			return nil, fmt.Errorf("%w: no container named %s", errMissing, name)
		}
		return nil, fmt.Errorf("%w: container %s is not running on a network", errConflict, c.name)
	}
	relayPort := uint32(guestwire.RelayFramedTCPPort)
	if network == "udp" {
		relayPort = guestwire.RelayUDPPort
	}
	dialCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	conn, err := dialPublishedPort(dialCtx, e.guestRelay, network, relayPort, netip.AddrPortFrom(target.ip, port))
	if err != nil {
		return nil, fmt.Errorf("connect to %s:%d: %w", name, port, err)
	}
	if network == "tcp" && e.guestRelay != nil {
		return guestwire.NewRelayTCPConn(conn), nil
	}
	return conn, nil
}

// bufferedConn reads what the HTTP server buffered before the hijack first.
type bufferedConn struct {
	net.Conn
	reader io.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) { return c.reader.Read(p) }

// pipe copies both ways until either side ends. A side that finishes
// sending half-closes the other when it can, so a request-reply exchange
// completes; then both close.
func pipe(a, b net.Conn) {
	var wg sync.WaitGroup
	copySide := func(dst, src net.Conn) {
		defer wg.Done()
		_, err := io.Copy(dst, src)
		if closer, ok := dst.(interface{ CloseWrite() error }); ok && err == nil {
			_ = closer.CloseWrite()
			return
		}
		_ = dst.Close()
		_ = src.Close()
	}
	wg.Add(2)
	go copySide(a, b)
	go copySide(b, a)
	wg.Wait()
}

// bridgeDatagrams relays between a framed client stream and a UDP socket,
// for engines that reach containers directly (Linux).
func bridgeDatagrams(client net.Conn, socket net.Conn) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, guestwire.MaxRelayDatagram)
		for {
			n, err := socket.Read(buf)
			if err != nil {
				_ = client.Close()
				return
			}
			if err := writeFrame(client, buf[:n]); err != nil {
				_ = socket.Close()
				return
			}
		}
	}()
	buf := make([]byte, guestwire.MaxRelayDatagram)
	for {
		n, err := guestwire.ReadRelayDatagram(client, buf)
		if err != nil {
			_ = socket.Close()
			break
		}
		if _, err := socket.Write(buf[:n]); err != nil && !errors.Is(err, net.ErrClosed) {
			continue
		}
	}
	<-done
}

// writeFrame writes one datagram frame: its length in two big-endian bytes,
// then the datagram.
func writeFrame(w io.Writer, data []byte) error {
	frame := make([]byte, 2+len(data))
	binary.BigEndian.PutUint16(frame, uint16(len(data)))
	copy(frame[2:], data)
	_, err := w.Write(frame)
	return err
}

// ContainerAt names the running container whose network address is ip, or
// returns "" when none is.
func (e *Engine) ContainerAt(ip netip.Addr) string {
	if e == nil || !ip.IsValid() {
		return ""
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, c := range e.containers {
		if c.state == "running" && c.attempt != nil && c.networkAddress() == ip {
			return c.name
		}
	}
	return ""
}
