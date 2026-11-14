//go:build linux || darwin

package localengine

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/OpenFabrica/FabriBay/engine/internal/guestwire"
)

func TestWaitNextExitFlushesBeforeStart(t *testing.T) {
	c := &container{id: "id", state: "created"}
	e := &Engine{containers: map[string]*container{c.id: c}}
	server := httptest.NewServer(e.Handler())
	defer server.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/v1.44/containers/id/wait?condition=next-exit", nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("wait registration: %s", res.Status)
	}

	e.mu.Lock()
	if c.attempt == nil {
		e.mu.Unlock()
		t.Fatal("wait was acknowledged before registering")
	}
	c.state = "running"
	e.mu.Unlock()
	e.onEvent(guestwire.RunEvent{Kind: guestwire.ExitedMessage, ID: c.id, Exit: guestwire.Exit{Code: 37}})
	var result struct{ StatusCode int }
	if err := json.NewDecoder(res.Body).Decode(&result); err != nil || result.StatusCode != 37 {
		t.Fatalf("wait result %+v: %v", result, err)
	}
}

// A client's TCP half-close must deliver guest EOF without closing its output.
func TestAttachHalfCloseKeepsOutput(t *testing.T) {
	t.Run("pipes", func(t *testing.T) { testAttachHalfCloseKeepsOutput(t, false) })
	t.Run("tty", func(t *testing.T) { testAttachHalfCloseKeepsOutput(t, true) })
}

func testAttachHalfCloseKeepsOutput(t *testing.T, tty bool) {
	c := &container{id: "id", state: "created", run: guestwire.RunSpec{Flags: guestwire.RunStdin}}
	expected := "\x01\x00\x00\x00\x00\x00\x00\x03bye"
	if tty {
		c.run.Flags |= guestwire.RunTTY
		expected = "bye"
	}
	e := &Engine{containers: map[string]*container{c.id: c}}
	host, guest := net.Pipe()
	defer guest.Close()
	deadline := time.Now().Add(3 * time.Second)
	_ = guest.SetDeadline(deadline)
	e.session = guestwire.NewSession(host, e.onEvent)
	defer e.session.Close()

	server := httptest.NewServer(e.Handler())
	defer server.Close()
	addr := server.Listener.Addr().(*net.TCPAddr)
	conn, err := net.DialTCP("tcp", nil, addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(deadline)
	if _, err := fmt.Fprint(conn, "POST /containers/id/attach?stdin=1&stdout=1&stream=1 HTTP/1.1\r\nHost: fabo\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(conn)
	res, err := http.ReadResponse(reader, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("attach status: %s", res.Status)
	}
	if _, err := io.WriteString(conn, "hi"); err != nil {
		t.Fatal(err)
	}
	if err := conn.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	e.mu.Lock()
	c.state = "starting"
	e.mu.Unlock()
	e.onEvent(guestwire.RunEvent{Kind: guestwire.StartedMessage, ID: c.id})
	var got strings.Builder
	for got.Len() < 2 {
		frame, err := guestwire.ReadFrame(guest, nil)
		if err != nil || frame.Kind != guestwire.InputMessage {
			t.Fatalf("input frame: %+v, %v", frame, err)
		}
		id, data, err := guestwire.DecodeInput(frame)
		if err != nil || id != c.id || len(data) == 0 {
			t.Fatalf("input: %q %q %v", id, data, err)
		}
		got.Write(data)
		// The guest may acknowledge partial pipe writes.
		for range data {
			ack, err := guestwire.InputConsumed(id, 1)
			if err != nil {
				t.Fatal(err)
			}
			if err := guestwire.WriteFrame(guest, ack); err != nil {
				t.Fatal(err)
			}
		}
	}
	if got.String() != "hi" {
		t.Fatalf("input = %q", got.String())
	}
	frame, err := guestwire.ReadFrame(guest, nil)
	if err != nil || frame.Kind != guestwire.CloseStdinMessage {
		t.Fatalf("EOF frame: %+v, %v", frame, err)
	}
	e.onEvent(guestwire.RunEvent{Kind: guestwire.OutputMessage, ID: c.id, Stream: 1, Data: []byte("bye")})
	e.onEvent(guestwire.RunEvent{Kind: guestwire.ExitedMessage, ID: c.id})
	body, err := io.ReadAll(reader)
	if err != nil || string(body) != expected {
		t.Fatalf("stdout after half-close: %q, %v", body, err)
	}
}
