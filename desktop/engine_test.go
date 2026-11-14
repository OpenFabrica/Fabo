package main

import (
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestReadOverviewUsesBackgroundQuery(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/info" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("X-Fabo-Background") != "1" {
			t.Error("request could wake the VM")
		}
		fmt.Fprint(w, `{"Name":"fabo","FaboVmAwake":false,"Containers":3,"ContainersRunning":1,"Images":2}`)
	}))
	defer server.Close()

	engine := newEngineClient(func(ctx context.Context) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", server.Listener.Addr().String())
	})
	info, err := engine.info(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if info.status() != "Asleep" || info.Containers != 3 || info.ContainersRunning != 1 || info.Images != 2 {
		t.Fatalf("unexpected overview: %+v", info)
	}
}

func TestLogText(t *testing.T) {
	raw := []byte{1, 0, 0, 0, 0, 0, 0, 3, 'o', 'u', 't', 2, 0, 0, 0, 0, 0, 0, 3, 'e', 'r', 'r'}
	if got := logText(raw, false); got != "outerr" {
		t.Fatalf("multiplexed output: %q", got)
	}
	if got := logText([]byte("line\n"), true); got != "line\n" {
		t.Fatalf("TTY output: %q", got)
	}
}

func TestEngineErrorUsesDaemonMessage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/fabo/stop" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("X-Fabo-Background") != "" {
			t.Error("explicit action was marked as background")
		}
		w.WriteHeader(http.StatusConflict)
		fmt.Fprint(w, `{"message":"engine is busy"}`)
	}))
	defer server.Close()
	engine := newEngineClient(func(ctx context.Context) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", server.Listener.Addr().String())
	})
	if err := engine.request(t.Context(), http.MethodPost, "/fabo/stop", nil, nil); err == nil || err.Error() != "engine is busy" {
		t.Fatalf("unexpected error: %v", err)
	}
}

// The engine answers an attached exec by upgrading the connection and then
// writing multiplexed frames until the command ends. exec must read that
// stream to its end and return the frames' payload.
func TestExecReadsTheUpgradedStream(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/exec/exec-1/json":
			fmt.Fprint(w, `{"Running":false,"ExitCode":0}`)
		case "/containers/api/exec":
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{"Id":"exec-1"}`)
		case "/exec/exec-1/start":
			if r.Header.Get("Upgrade") != "tcp" {
				t.Errorf("start did not ask for an upgrade: %v", r.Header)
			}
			conn, buf, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			defer conn.Close()
			buf.WriteString("HTTP/1.1 101 UPGRADED\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n")
			for stream, text := range map[byte]string{1: "bin/\n"} {
				head := make([]byte, 8)
				head[0] = stream
				binary.BigEndian.PutUint32(head[4:], uint32(len(text)))
				buf.Write(head)
				buf.WriteString(text)
			}
			buf.Flush()
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL)
		}
	}))
	defer server.Close()
	engine := newEngineClient(func(ctx context.Context) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", server.Listener.Addr().String())
	})
	out, err := engine.exec(t.Context(), "api", []string{"ls", "-1Ap", "--", "/"})
	if err != nil || out != "bin/\n" {
		t.Fatalf("exec output %q, %v", out, err)
	}
}
