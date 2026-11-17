//go:build !windows

package wslbridge

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestFrameBytes pins the wire layout shared with upstream Kiri's bridge.
func TestFrameBytes(t *testing.T) {
	var b bytes.Buffer
	if err := WriteFrame(&b, KindData, 7, []byte("abc")); err != nil {
		t.Fatal(err)
	}
	want := []byte{2, 0, 0, 0, 7, 0, 0, 0, 3, 'a', 'b', 'c'}
	if !bytes.Equal(b.Bytes(), want) {
		t.Fatalf("frame = %v, want %v", b.Bytes(), want)
	}
	WriteFrame(&b, KindEOF, 7, nil)
	f, err := ReadFrame(&b)
	if err != nil || f.Kind != KindData || f.ID != 7 || string(f.Data) != "abc" {
		t.Fatalf("first frame = %+v, %v", f, err)
	}
	if f, err = ReadFrame(&b); err != nil || f.Kind != KindEOF || len(f.Data) != 0 {
		t.Fatalf("second frame = %+v, %v", f, err)
	}
	if _, err = ReadFrame(&b); !errors.Is(err, io.EOF) {
		t.Fatalf("end = %v, want io.EOF", err)
	}
}

// TestOversizedFrames refuses a huge length before allocating it.
func TestOversizedFrames(t *testing.T) {
	var b bytes.Buffer
	WriteFrame(&b, KindData, 7, make([]byte, MaxFrame))
	if f, err := ReadFrame(&b); err != nil || len(f.Data) != MaxFrame {
		t.Fatalf("MaxFrame frame: %d bytes, %v", len(f.Data), err)
	}
	h := header(KindData, 7, int(^uint32(0)))
	if _, err := ReadFrame(bytes.NewReader(h[:])); !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("4 GiB frame: %v", err)
	}
	// A frame cut short is not a clean end.
	h = header(KindData, 7, 10)
	if _, err := ReadFrame(bytes.NewReader(append(h[:], 1, 2))); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("short frame: %v", err)
	}
}

// daemonSocket serves one handler per connection on a Unix socket in a
// short temporary directory and returns a dialer for it.
func daemonSocket(t *testing.T, handle func(*net.UnixConn)) func() (Conn, error) {
	t.Helper()
	dir, err := os.MkdirTemp("", "wb")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	path := filepath.Join(dir, "d.sock")
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.AcceptUnix()
			if err != nil {
				return
			}
			go handle(c)
		}
	}()
	return func() (Conn, error) {
		c, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: path, Net: "unix"})
		if err != nil {
			return nil, err
		}
		return c, nil
	}
}

// rawBridge runs Serve between socket pairs and returns the host's ends:
// what it writes to the bridge and what it reads from it.
func rawBridge(t *testing.T, dial func() (Conn, error)) (io.WriteCloser, io.Reader) {
	t.Helper()
	// OS pipes, buffered as wsl.exe's stdio is: the bridge's hello and the
	// test's first frame must not wait for each other.
	inR, inW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		Serve(inR, outW, dial)
		outW.Close()
	}()
	t.Cleanup(func() { inW.Close(); outR.Close() })
	return inW, outR
}

// TestSlowStdinWaitsForCredit: `docker run -i … wc -c < big file` where the
// container reads slower than input comes. The input waits for credit, all
// of it arrives, and the connection stays open.
func TestSlowStdinWaitsForCredit(t *testing.T) {
	// Eight windows: enough to wait for credit many times over.
	const total = 8 * Window
	dial := daemonSocket(t, func(c *net.UnixConn) {
		buf := make([]byte, 256<<10)
		n := 0
		for {
			k, err := c.Read(buf)
			n += k
			if err != nil {
				break
			}
			time.Sleep(time.Millisecond)
		}
		c.Write([]byte(strconv.Itoa(n)))
		c.Close()
	})
	to, from := rawBridge(t, dial)
	window := newCredit()
	type result struct {
		hello bool
		reply string
		acks  int
	}
	done := make(chan result, 1)
	go func() {
		var r result
		var reply []byte
		for {
			f, err := ReadFrame(from)
			if err != nil {
				t.Errorf("read: %v", err)
				break
			}
			switch {
			case f.Kind == KindAck && f.ID == 0:
				r.hello = true
			case f.Kind == KindAck && f.ID == 1:
				n, _ := ackCount(f.Data)
				r.acks += n
				window.ack(n)
			case f.Kind == KindData && f.ID == 1:
				reply = append(reply, f.Data...)
			case f.Kind == KindEOF && f.ID == 1:
				r.reply = string(reply)
				done <- r
				return
			default:
				t.Errorf("unexpected frame %+v", f)
			}
		}
		done <- r
	}()
	WriteFrame(to, KindOpen, 1, []byte{1})
	chunk := bytes.Repeat([]byte{7}, 64<<10)
	for sent := 0; sent < total; sent += len(chunk) {
		if !window.take(len(chunk), nil) {
			t.Fatalf("closed after %d bytes", sent)
		}
		WriteFrame(to, KindData, 1, chunk)
	}
	WriteFrame(to, KindEOF, 1, nil)
	r := <-done
	if !r.hello || r.reply != strconv.Itoa(total) {
		t.Fatalf("hello %v, reply %q", r.hello, r.reply)
	}
	// Everything acknowledged but the last part under AckEvery.
	if r.acks+AckEvery <= total || r.acks > total {
		t.Fatalf("acknowledged %d of %d", r.acks, total)
	}
}

// TestSlowReaderHoldsOutputToTheWindow: a client that reads its output
// slowly holds the bridge to a window of it in flight.
func TestSlowReaderHoldsOutputToTheWindow(t *testing.T) {
	const total = 8 * Window
	dial := daemonSocket(t, func(c *net.UnixConn) {
		chunk := bytes.Repeat([]byte{1}, 64<<10)
		for i := 0; i < total/len(chunk); i++ {
			if _, err := c.Write(chunk); err != nil {
				break
			}
		}
		c.Close()
	})
	to, from := rawBridge(t, dial)
	WriteFrame(to, KindOpen, 1, []byte{1})
	got, acked, most := 0, 0, 0
	for {
		f, err := ReadFrame(from)
		if err != nil {
			t.Fatal(err)
		}
		if f.Kind == KindAck && f.ID == 0 {
			continue
		}
		if f.Kind == KindEOF && f.ID == 1 {
			break
		}
		if f.Kind != KindData || f.ID != 1 {
			t.Fatalf("unexpected frame %+v", f)
		}
		got += len(f.Data)
		most = max(most, got-acked)
		time.Sleep(200 * time.Microsecond)
		if got-acked >= AckEvery {
			WriteFrame(to, KindAck, 1, ackData(got-acked))
			acked = got
		}
	}
	if got != total {
		t.Fatalf("got %d of %d", got, total)
	}
	if most > Window+64<<10 {
		t.Fatalf("%d bytes in flight", most)
	}
}

// TestConnectionsWithoutCredit: a host that asks for no flow control (as
// upstream's before it) gets no ACKs for its connections and works.
func TestConnectionsWithoutCredit(t *testing.T) {
	dial := daemonSocket(t, func(c *net.UnixConn) {
		req, _ := io.ReadAll(c)
		c.Write(req)
		c.Close()
	})
	to, from := rawBridge(t, dial)
	WriteFrame(to, KindOpen, 1, nil)
	WriteFrame(to, KindData, 1, []byte("ping"))
	WriteFrame(to, KindEOF, 1, nil)
	var reply []byte
	for {
		f, err := ReadFrame(from)
		if err != nil {
			t.Fatal(err)
		}
		if f.Kind == KindAck && f.ID == 0 {
			continue
		}
		if f.Kind == KindEOF {
			break
		}
		if f.Kind != KindData || f.ID != 1 {
			t.Fatalf("unexpected frame %+v", f)
		}
		reply = append(reply, f.Data...)
	}
	if string(reply) != "ping" {
		t.Fatalf("reply %q", reply)
	}
}

// TestDialFailureCloses answers OPEN with CLOSE when the daemon is down.
func TestDialFailureCloses(t *testing.T) {
	to, from := rawBridge(t, func() (Conn, error) { return nil, errors.New("no daemon") })
	WriteFrame(to, KindOpen, 9, []byte{1})
	for {
		f, err := ReadFrame(from)
		if err != nil {
			t.Fatal(err)
		}
		if f.Kind == KindClose && f.ID == 9 {
			return
		}
	}
}

// muxPair is a Mux whose bridges run Serve in-process against dial.
type muxPair struct {
	mux    *Mux
	starts int
	kill   func()
}

// newMux builds a Mux over an in-process bridge.
func newMux(t *testing.T, dial func() (Conn, error)) *muxPair {
	p := &muxPair{}
	p.mux = &Mux{
		Logf: t.Logf,
		Start: func() (*Transport, error) {
			p.starts++
			inR, inW := io.Pipe()
			outR, outW := io.Pipe()
			exited := make(chan error, 1)
			go func() {
				err := Serve(inR, outW, dial)
				outW.CloseWithError(io.EOF)
				exited <- err
			}()
			p.kill = func() { inR.CloseWithError(io.ErrClosedPipe); outW.CloseWithError(io.EOF) }
			return &Transport{In: inW, Out: outR, Wait: func() error { return <-exited }}, nil
		},
	}
	return p
}

// clientPair returns a connected client end and the end the Mux serves,
// both Unix sockets, so half-closes behave as on a named pipe.
func clientPair(t *testing.T) (*net.UnixConn, *net.UnixConn) {
	t.Helper()
	dir, err := os.MkdirTemp("", "wc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	addr := &net.UnixAddr{Name: filepath.Join(dir, "c.sock"), Net: "unix"}
	l, err := net.ListenUnix("unix", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	accepted := make(chan *net.UnixConn, 1)
	go func() {
		c, _ := l.AcceptUnix()
		accepted <- c
	}()
	client, err := net.DialUnix("unix", nil, addr)
	if err != nil {
		t.Fatal(err)
	}
	server := <-accepted
	t.Cleanup(func() { client.Close() })
	return client, server
}

// TestMuxHalfClose: the client half-closes its request and still reads the
// whole reply, then EOF, through both ends of the bridge.
func TestMuxHalfClose(t *testing.T) {
	dial := daemonSocket(t, func(c *net.UnixConn) {
		req, _ := io.ReadAll(c)
		c.Write([]byte("got " + string(req)))
		c.CloseWrite()
		io.Copy(io.Discard, c)
		c.Close()
	})
	p := newMux(t, dial)
	for i := range 3 {
		client, server := clientPair(t)
		done := make(chan struct{})
		go func() { p.mux.Serve(server); close(done) }()
		fmt.Fprintf(client, "request %d", i)
		client.CloseWrite()
		reply, err := io.ReadAll(client)
		if err != nil || string(reply) != fmt.Sprintf("got request %d", i) {
			t.Fatalf("reply %q, %v", reply, err)
		}
		client.Close()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("Serve did not return after both half-closes")
		}
	}
	if p.starts != 1 {
		t.Fatalf("%d bridges started for three connections", p.starts)
	}
	// Both sides forgot the finished connections.
	b := p.mux.current
	b.mu.Lock()
	n := len(b.conns)
	b.mu.Unlock()
	if n != 0 {
		t.Fatalf("%d connections left on the Windows side", n)
	}
}

// TestMuxBridgeDeath: a bridge that dies with a connection open ends the
// client's connection, and the next connection starts a new bridge.
func TestMuxBridgeDeath(t *testing.T) {
	hold := make(chan struct{})
	t.Cleanup(func() { close(hold) })
	dial := daemonSocket(t, func(c *net.UnixConn) {
		buf := make([]byte, 4)
		io.ReadFull(c, buf)
		c.Write([]byte("pong"))
		<-hold
		c.Close()
	})
	p := newMux(t, dial)
	client, server := clientPair(t)
	go p.mux.Serve(server)
	client.Write([]byte("ping"))
	buf := make([]byte, 4)
	if _, err := io.ReadFull(client, buf); err != nil || string(buf) != "pong" {
		t.Fatalf("first reply %q, %v", buf, err)
	}
	p.kill()
	client.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := client.Read(buf); err == nil {
		t.Fatal("client still connected after the bridge died")
	} else if errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatal("client left waiting after the bridge died")
	}
	// The dead bridge is noticed, and a new one carries the next client.
	deadline := time.Now().Add(5 * time.Second)
	for !p.mux.current.dead.Load() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	client2, server2 := clientPair(t)
	go p.mux.Serve(server2)
	client2.Write([]byte("ping"))
	if _, err := io.ReadFull(client2, buf); err != nil || string(buf) != "pong" {
		t.Fatalf("second reply %q, %v", buf, err)
	}
	if p.starts != 2 {
		t.Fatalf("%d bridges started", p.starts)
	}
}

// TestMuxNotSetUp: with no distro yet, a client is told setup is unfinished
// rather than having its connection dropped.
func TestMuxNotSetUp(t *testing.T) {
	m := &Mux{
		Start:  func() (*Transport, error) { return nil, ErrNotSetUp },
		Refuse: func(c Conn) bool { return RefuseHTTP(c, "Fabo's setup isn't finished") },
	}
	client, server := clientPair(t)
	go m.Serve(server)
	client.Write([]byte("GET /_ping HTTP/1.1\r\nHost: docker\r\n\r\n"))
	reply, _ := io.ReadAll(client)
	if !strings.HasPrefix(string(reply), "HTTP/1.1 503 ") || !strings.Contains(string(reply), `{"message":"Fabo's setup isn't finished"}`) {
		t.Fatalf("reply %q", reply)
	}
}

// TestServeForgetsEndedConnections: a connection that ends with EOF both
// ways is released on the WSL side although the host sends no CLOSE
// (upstream kept each one until the bridge ended).
func TestServeForgetsEndedConnections(t *testing.T) {
	dial := daemonSocket(t, func(c *net.UnixConn) {
		io.ReadAll(c)
		c.Write([]byte("ok"))
		c.Close()
	})
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	s := newServer(outW)
	served := make(chan struct{})
	go func() { s.run(inR, dial); close(served) }()
	eofs := make(chan uint32, 64)
	go func() {
		for {
			f, err := ReadFrame(outR)
			if err != nil {
				return
			}
			if f.Kind == KindEOF {
				eofs <- f.ID
			}
		}
	}()
	const n = 50
	for id := uint32(1); id <= n; id++ {
		WriteFrame(inW, KindOpen, id, []byte{1})
		WriteFrame(inW, KindEOF, id, nil)
	}
	for range n {
		select {
		case <-eofs:
		case <-time.After(5 * time.Second):
			t.Fatal("missing replies")
		}
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		s.mu.Lock()
		left := len(s.conns)
		s.mu.Unlock()
		if left == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d connections still held", left)
		}
		time.Sleep(10 * time.Millisecond)
	}
	inW.Close()
	select {
	case <-served:
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return")
	}
}
