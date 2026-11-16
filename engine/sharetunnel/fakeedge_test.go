package sharetunnel

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/OpenFabrica/Fabo/engine/sharewire"
)

// fakeEdge is a minimal relay written from upstream's rules, without the
// shared Stream code, so a flow-control mistake made the same way in the
// client and in fabo-edge still fails here. Each accepted session runs
// script with a frame reader and writer.
type fakeEdge struct {
	addr string
	ca   *testCA
}

// fakeConn is one session as the fake relay sees it.
type fakeConn struct {
	t   *testing.T
	c   *tls.Conn
	br  *bufio.Reader
	wmu sync.Mutex
	buf []byte
}

func (f *fakeConn) send(m sharewire.Msg) error {
	f.wmu.Lock()
	defer f.wmu.Unlock()
	_ = f.c.SetWriteDeadline(time.Now().Add(10 * time.Second))
	_, err := f.c.Write(sharewire.Append(nil, m))
	return err
}

func (f *fakeConn) recv() (sharewire.Msg, error) {
	_ = f.c.SetReadDeadline(time.Now().Add(20 * time.Second))
	frame, err := sharewire.ReadFrame(f.br, f.buf)
	if err != nil {
		return nil, err
	}
	f.buf = frame
	m, err := sharewire.Decode(frame)
	if d, ok := m.(sharewire.Data); ok {
		d.Data = append([]byte(nil), d.Data...)
		m = d
	}
	return m, err
}

func startFakeEdge(t *testing.T, script func(f *fakeConn, session int)) *fakeEdge {
	ca := newTestCA(t)
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{ca.Cert}, NextProtos: []string{sharewire.ALPN}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for n := 0; ; n++ {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				f := &fakeConn{t: t, c: c.(*tls.Conn), br: bufio.NewReader(c)}
				m, err := f.recv()
				if _, ok := m.(sharewire.Hello); err != nil || !ok {
					return
				}
				script(f, n)
			}()
		}
	}()
	return &fakeEdge{addr: ln.Addr().String(), ca: ca}
}

func (e *fakeEdge) options(t *testing.T, target string, rec *recorder) Options {
	t.Setenv("FABO_SHARE_CA", e.ca.Path)
	t.Setenv("FABO_SHARE_CONNECT", e.addr)
	srv, err := ParseServer(testEdgeHost)
	if err != nil {
		t.Fatal(err)
	}
	return Options{Server: srv, Token: "ft_fake", Device: "d", Client: "fabo test",
		Target: Target{Addr: target, Display: target}, Dial: DialAddr(target), OnEvent: rec.on}
}

// TestFakeEdgeUploadAndDownload sends a 1MB request and reads a 1MB reply,
// with credit kept by hand: the relay may have InitialWindow unacknowledged
// toward the client, and grants the client credit as it consumes Data.
func TestFakeEdgeUploadAndDownload(t *testing.T) {
	local := echoServer(t)
	body := randomBytes(1 << 20)
	result := make(chan error, 1)
	fake := startFakeEdge(t, func(f *fakeConn, _ int) {
		result <- func() error {
			if err := f.send(sharewire.Welcome{URL: "https://web-abc123." + testDomain}); err != nil {
				return err
			}
			if err := f.send(sharewire.Open{Stream: 1, Remote: "203.0.113.9:5000"}); err != nil {
				return err
			}
			// Ping must be answered with the same value.
			if err := f.send(sharewire.Ping{Value: 77}); err != nil {
				return err
			}
			var mu sync.Mutex
			cond := sync.NewCond(&mu)
			credit := int(sharewire.InitialWindow)
			var reply bytes.Buffer
			closed, pong := false, false
			readErr := make(chan error, 1)
			go func() {
				for {
					m, err := f.recv()
					if err != nil {
						readErr <- err
						return
					}
					mu.Lock()
					switch m := m.(type) {
					case sharewire.Window:
						credit += int(m.Credit)
						if credit > int(sharewire.InitialWindow) {
							mu.Unlock()
							readErr <- fmt.Errorf("client returned %d credit, more than the window", credit)
							return
						}
					case sharewire.Data:
						if len(m.Data) > sharewire.MaxChunk {
							mu.Unlock()
							readErr <- fmt.Errorf("Data of %d bytes", len(m.Data))
							return
						}
						reply.Write(m.Data)
						mu.Unlock()
						// Consumed at once: grant the same back.
						if err := f.send(sharewire.Window{Stream: 1, Credit: uint32(len(m.Data))}); err != nil {
							readErr <- err
							return
						}
						mu.Lock()
					case sharewire.Close:
						closed = true
					case sharewire.Pong:
						pong = m.Value == 77
					case sharewire.Reset:
						mu.Unlock()
						readErr <- fmt.Errorf("client reset the stream")
						return
					}
					cond.Broadcast()
					done := closed
					mu.Unlock()
					if done {
						readErr <- nil
						return
					}
				}
			}()
			request := append([]byte("POST /up HTTP/1.1\r\nHost: web\r\nConnection: close\r\nContent-Length: "+strconv.Itoa(len(body))+"\r\n\r\n"), body...)
			for len(request) > 0 {
				mu.Lock()
				for credit == 0 {
					cond.Wait()
				}
				n := min(credit, sharewire.MaxChunk, len(request))
				credit -= n
				mu.Unlock()
				if err := f.send(sharewire.Data{Stream: 1, Data: request[:n]}); err != nil {
					return err
				}
				request = request[n:]
			}
			if err := <-readErr; err != nil {
				return err
			}
			resp, err := http.ReadResponse(bufio.NewReader(bytes.NewReader(reply.Bytes())), nil)
			if err != nil {
				return err
			}
			got, _ := io.ReadAll(resp.Body)
			if !bytes.Equal(got, body) {
				return fmt.Errorf("echoed %d of %d bytes", len(got), len(body))
			}
			if !pong {
				return fmt.Errorf("Ping was not answered")
			}
			return f.send(sharewire.Close{Stream: 1})
		}()
	})
	rec := newRecorder()
	h := Start(fake.options(t, local.Listener.Addr().String(), rec))
	defer h.Stop()
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("fake relay exchange timed out")
	}
}

// TestStopWhenRelayStopsReading: the relay stops reading while the local
// service floods; Stop must not hang on the stalled write.
func TestStopWhenRelayStopsReading(t *testing.T) {
	flood := rawServer(t, func(c net.Conn) {
		defer c.Close()
		chunk := randomBytes(64 << 10)
		for {
			if _, err := c.Write(chunk); err != nil {
				return
			}
		}
	})
	hold := make(chan struct{})
	t.Cleanup(func() { close(hold) })
	fake := startFakeEdge(t, func(f *fakeConn, _ int) {
		// A 64MiB window lets the client send far more than the socket
		// buffers hold; then the relay never reads again.
		_ = f.send(sharewire.Welcome{URL: "https://flood-abc123." + testDomain, Window: 64 << 20})
		_ = f.send(sharewire.Open{Stream: 1})
		_ = f.send(sharewire.Data{Stream: 1, Data: []byte("GET / HTTP/1.1\r\n\r\n")})
		<-hold
	})
	rec := newRecorder()
	h := Start(fake.options(t, flood, rec))
	rec.wait(t, EventLive, 5*time.Second)
	time.Sleep(500 * time.Millisecond) // let the socket buffers fill
	start := time.Now()
	h.Stop()
	if d := time.Since(start); d > time.Second {
		t.Fatalf("Stop took %v with a stalled relay", d)
	}
}

// TestBadCreditReconnects: zero credit is a protocol error.
func TestBadCreditReconnects(t *testing.T) {
	fake := startFakeEdge(t, func(f *fakeConn, n int) {
		_ = f.send(sharewire.Welcome{URL: "https://web-abc123." + testDomain})
		if n == 0 {
			_ = f.send(sharewire.Open{Stream: 1})
			_ = f.send(sharewire.Window{Stream: 1, Credit: 0})
		}
		for {
			if _, err := f.recv(); err != nil {
				return
			}
		}
	})
	local := echoServer(t)
	rec := newRecorder()
	h := Start(fake.options(t, local.Listener.Addr().String(), rec))
	defer h.Stop()
	r := rec.wait(t, EventReconnecting, 5*time.Second)
	if !strings.Contains(r.Reason, "bad credit") {
		t.Fatalf("reconnect reason %q", r.Reason)
	}
	// Reconnecting asks for the issued name and goes live again.
	if live := rec.wait(t, EventLive, 10*time.Second); live.URL == "" {
		t.Fatal("no second session")
	}
}

// TestNotAShareRelay: a server that doesn't select the share ALPN ends the
// tunnel for good.
func TestNotAShareRelay(t *testing.T) {
	ca := newTestCA(t)
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{ca.Cert}})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { _ = c.(*tls.Conn).Handshake(); time.Sleep(time.Second); c.Close() }()
		}
	}()
	fake := &fakeEdge{addr: ln.Addr().String(), ca: ca}
	rec := newRecorder()
	h := Start(fake.options(t, "127.0.0.1:1", rec))
	defer h.Stop()
	if e := rec.wait(t, EventEnded, 5*time.Second); !e.Error || !strings.Contains(e.Reason, "isn't a Fabo share relay") {
		t.Fatalf("ended %+v", e)
	}
}
