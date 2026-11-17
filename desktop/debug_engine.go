package main

// The engine side of the Debug page: a connection to a container's port
// through the engine (POST /fabo/debug/dial), which works for ports that
// are not published, and the name of the container behind a connection the
// engine relayed to this computer (GET /fabo/debug/peers).

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// debugMaxDatagram is the largest UDP payload the engine relays, as the
// guest's relay frames carry.
const debugMaxDatagram = 65507

// errOldEngine says the engine predates the debug endpoints: a window
// sharing an installed engine may talk to one. It is phrased when it
// happens, in the window's language at that moment.
func errOldEngine() error {
	return errors.New(tr("引擎版本较旧，不支持容器网络直连，请改用映射端口或更新 Fabo", "The engine is too old to reach the container network; use the published port or update Fabo"))
}

// dialContainer opens a stream (tcp) or a datagram channel (udp) to a
// container's port through the engine.
func (e engineClient) dialContainer(ctx context.Context, name string, port int, network string) (net.Conn, error) {
	if e.http == nil {
		return nil, errors.New(tr("没有连接到引擎", "Not connected to the engine"))
	}
	query := url.Values{"container": {name}, "port": {strconv.Itoa(port)}, "proto": {network}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://localhost/fabo/debug/dial?"+query.Encode(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "tcp")
	res, err := e.http.Do(req)
	if err != nil {
		return nil, err
	}
	if res.StatusCode != http.StatusSwitchingProtocols {
		defer res.Body.Close()
		// The new endpoint marks its own answers; a 404 without the mark
		// comes from an engine that does not have it.
		if res.StatusCode == http.StatusNotFound && res.Header.Get("X-Fabo-Debug") == "" {
			return nil, errOldEngine()
		}
		return nil, responseError(res)
	}
	stream, ok := res.Body.(io.ReadWriteCloser)
	if !ok {
		res.Body.Close()
		return nil, errors.New("the engine did not hand over the connection")
	}
	remote := debugAddr(fmt.Sprintf("%s:%d", name, port))
	if network == debugUDP {
		return &datagramConn{streamConn: streamConn{stream: stream, remote: remote}}, nil
	}
	return &streamConn{stream: stream, remote: remote}, nil
}

// debugAddr is a container target as a net.Addr.
type debugAddr string

func (a debugAddr) Network() string { return "fabo" }
func (a debugAddr) String() string  { return string(a) }

// streamConn is an upgraded engine connection as a net.Conn. Deadlines are
// not supported by the stream; closing it is what ends a blocked read.
type streamConn struct {
	stream io.ReadWriteCloser
	remote net.Addr
}

func (c *streamConn) Read(p []byte) (int, error)       { return c.stream.Read(p) }
func (c *streamConn) Write(p []byte) (int, error)      { return c.stream.Write(p) }
func (c *streamConn) Close() error                     { return c.stream.Close() }
func (c *streamConn) LocalAddr() net.Addr              { return debugAddr("engine") }
func (c *streamConn) RemoteAddr() net.Addr             { return c.remote }
func (c *streamConn) SetDeadline(time.Time) error      { return nil }
func (c *streamConn) SetReadDeadline(time.Time) error  { return nil }
func (c *streamConn) SetWriteDeadline(time.Time) error { return nil }

// datagramConn carries UDP datagrams over an engine stream, each framed by
// its length in two big-endian bytes, as the guest's UDP relay frames them.
type datagramConn struct {
	streamConn
	writeMu sync.Mutex
}

func (c *datagramConn) Read(p []byte) (int, error) {
	var header [2]byte
	if _, err := io.ReadFull(c.stream, header[:]); err != nil {
		return 0, err
	}
	n := int(binary.BigEndian.Uint16(header[:]))
	if n > len(p) {
		// Read and drop a datagram too large for the buffer, as a socket
		// would truncate it.
		if _, err := io.CopyN(io.Discard, c.stream, int64(n)); err != nil {
			return 0, err
		}
		return 0, fmt.Errorf("datagram of %d bytes exceeds the buffer", n)
	}
	return io.ReadFull(c.stream, p[:n])
}

func (c *datagramConn) Write(p []byte) (int, error) {
	if len(p) > debugMaxDatagram {
		return 0, fmt.Errorf("datagram exceeds %d bytes", debugMaxDatagram)
	}
	frame := make([]byte, 2+len(p))
	binary.BigEndian.PutUint16(frame, uint16(len(p)))
	copy(frame[2:], p)
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if _, err := c.stream.Write(frame); err != nil {
		return 0, err
	}
	return len(p), nil
}

// debugPeer is what GET /fabo/debug/peers says about a relayed connection.
type debugPeerInfo struct {
	Source    string `json:"source"`
	Container string `json:"container"`
}

// debugPeers asks the engine which containers are behind connections it
// relayed to this computer, keyed by the local address the relay connected
// from (what a server here sees as the peer). An engine without the
// endpoint, or without a relay (Linux), answers nothing.
func (e engineClient) debugPeers(ctx context.Context, proto string, addrs []string) (map[string]debugPeerInfo, error) {
	if e.http == nil || len(addrs) == 0 {
		return nil, nil
	}
	query := url.Values{"proto": {proto}, "addr": addrs}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://localhost/fabo/debug/peers?"+query.Encode(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Fabo-Background", "1")
	res, err := e.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	if res.StatusCode != http.StatusOK {
		return nil, responseError(res)
	}
	out := map[string]debugPeerInfo{}
	if err := json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&out); err != nil {
		return nil, err
	}
	return out, nil
}

// loopbackPeer reports whether a peer came through the engine's relay to
// this computer's loopback, the ones the engine can name.
func loopbackPeer(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	return err == nil && (host == "127.0.0.1" || strings.HasPrefix(host, "127."))
}
