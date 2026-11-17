//go:build linux || darwin

package localengine

import (
	"bufio"
	"encoding/binary"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/OpenFabrica/Fabo/engine/internal/guestwire"
)

// debugEngine is an engine with one running container whose bridge
// address is the loopback, so the direct (Linux) dial path reaches
// listeners of the test.
func debugEngine(t *testing.T) *httptest.Server {
	t.Helper()
	c := &container{id: "redis0123456789", name: "redis", state: "running", attempt: &runAttempt{done: make(chan struct{})}}
	c.run.Network.Bridge = "fabo0"
	c.run.Network.IP = [4]byte{127, 0, 0, 1}
	stopped := &container{id: "old0123456789", name: "old", state: "exited"}
	e := &Engine{containers: map[string]*container{c.id: c, stopped.id: stopped}}
	server := httptest.NewServer(e.Handler())
	t.Cleanup(server.Close)
	return server
}

// upgrade asks the engine for a debug connection and returns the stream.
func upgrade(t *testing.T, server *httptest.Server, query string) (net.Conn, *bufio.Reader, *http.Response) {
	t.Helper()
	conn, err := net.Dial("tcp", strings.TrimPrefix(server.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	_, err = io.WriteString(conn, "POST /fabo/debug/dial?"+query+" HTTP/1.1\r\nHost: localhost\r\nConnection: Upgrade\r\nUpgrade: tcp\r\nContent-Length: 0\r\n\r\n")
	if err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(conn)
	res, err := http.ReadResponse(reader, nil)
	if err != nil {
		t.Fatal(err)
	}
	return conn, reader, res
}

func TestDebugDialTCP(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		_, _ = io.Copy(c, c)
	}()
	server := debugEngine(t)
	port := ln.Addr().(*net.TCPAddr).Port
	conn, reader, res := upgrade(t, server, "container=redis&proto=tcp&port="+strconv.Itoa(port))
	if res.StatusCode != http.StatusSwitchingProtocols || res.Header.Get(debugMark) != "1" {
		t.Fatalf("status %d, mark %q", res.StatusCode, res.Header.Get(debugMark))
	}
	if _, err := io.WriteString(conn, "PING\r\n"); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	line, err := reader.ReadString('\n')
	if err != nil || line != "PING\r\n" {
		t.Fatalf("echo %q, %v", line, err)
	}
}

func TestDebugDialUDP(t *testing.T) {
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	go func() {
		buf := make([]byte, 1500)
		for {
			n, from, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			_, _ = pc.WriteTo(append([]byte("ACK "), buf[:n]...), from)
		}
	}()
	server := debugEngine(t)
	port := pc.LocalAddr().(*net.UDPAddr).Port
	conn, reader, res := upgrade(t, server, "container=redis&proto=udp&port="+strconv.Itoa(port))
	if res.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("status %d", res.StatusCode)
	}
	if err := guestwire.WriteRelayDatagram(conn, []byte("metric:1|c")); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	var header [2]byte
	if _, err := io.ReadFull(reader, header[:]); err != nil {
		t.Fatal(err)
	}
	body := make([]byte, binary.BigEndian.Uint16(header[:]))
	if _, err := io.ReadFull(reader, body); err != nil || string(body) != "ACK metric:1|c" {
		t.Fatalf("reply %q, %v", body, err)
	}
}

func TestDebugDialRefusals(t *testing.T) {
	server := debugEngine(t)
	for query, want := range map[string]int{
		"container=missing&port=80":        http.StatusNotFound,
		"container=old&port=80":            http.StatusConflict,
		"container=redis&port=0":           http.StatusBadRequest,
		"container=redis&port=80&proto=ip": http.StatusBadRequest,
	} {
		_, _, res := upgrade(t, server, query)
		if res.StatusCode != want || res.Header.Get(debugMark) != "1" {
			t.Errorf("%s: status %d mark %q, want %d", query, res.StatusCode, res.Header.Get(debugMark), want)
		}
	}
}
