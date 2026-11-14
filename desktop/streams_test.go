package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/egoist/mygo/plugins/terminal"
	"github.com/egoist/mygo/ui"
)

func waitStream(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("stream condition timed out")
		}
		time.Sleep(time.Millisecond)
	}
}
func streamClient(server *httptest.Server) engineClient {
	return newEngineClient(func(ctx context.Context) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", server.Listener.Addr().String())
	})
}
func framed(text []byte) []byte {
	header := make([]byte, 8)
	header[0] = 1
	binary.BigEndian.PutUint32(header[4:], uint32(len(text)))
	return append(header, text...)
}

func TestDockerStreamHandlesSplitFramesAndTruncation(t *testing.T) {
	data := append(framed([]byte("one")), framed([]byte("你好\n"))...)
	r := &dockerReader{reader: &oneByteReader{bytes.NewReader(data)}}
	out, err := io.ReadAll(r)
	if err != nil || string(out) != "one你好\n" {
		t.Fatalf("decoded %q %v", out, err)
	}
	_, err = io.ReadAll(&dockerReader{reader: bytes.NewReader(framed([]byte("lost"))[:10])})
	if err != io.ErrUnexpectedEOF {
		t.Fatalf("truncation accepted: %v", err)
	}
}

type oneByteReader struct{ reader io.Reader }

func (r *oneByteReader) Read(p []byte) (int, error) {
	if len(p) > 1 {
		p = p[:1]
	}
	return r.reader.Read(p)
}

func TestContainerLogStreamFollowsPartialLinesAndCancels(t *testing.T) {
	disconnected := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Kiri-Background") != "1" {
			t.Error("log read keeps VM awake")
		}
		switch r.URL.Path {
		case "/containers/test/json":
			fmt.Fprint(w, `{"Config":{"Tty":false}}`)
		case "/containers/test/logs":
			if r.URL.Query().Get("follow") != "1" || r.URL.Query().Get("tail") != "5000" {
				t.Error("not following")
			}
			w.WriteHeader(200)
			for _, p := range [][]byte{{0xe4, 0xbd}, {0xa0, '\n'}, []byte("unfinished")} {
				w.Write(framed(p))
				w.(http.Flusher).Flush()
			}
			<-r.Context().Done()
			close(disconnected)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	engine := streamClient(server)
	logs := newLiveLogs(t.Context(), "container", func() {}, func(ctx context.Context) (io.ReadCloser, func() error, error) {
		return engine.openContainerLogs(ctx, "test")
	})
	defer logs.close()
	waitStream(t, func() bool { return strings.Join(logs.snapshot().lines, "|") == "你|unfinished" })
	logs.close()
	select {
	case <-logs.done:
	case <-time.After(3 * time.Second):
		t.Fatal("quiet stream did not close")
	}
	select {
	case <-disconnected:
	case <-time.After(3 * time.Second):
		t.Fatal("HTTP subscription leaked")
	}
}
func TestLogBufferIsBoundedAndKeepsLatestLines(t *testing.T) {
	logs := &liveLogs{}
	for i := 0; i < maxLiveLogLines+20; i++ {
		logs.append([]byte(fmt.Sprintf("line %d\n", i)))
	}
	snapshot := logs.snapshot()
	if len(snapshot.lines) != maxLiveLogLines || snapshot.lines[0] != "line 20" {
		t.Fatalf("retention %d %q", len(snapshot.lines), snapshot.lines[0])
	}
	logs.append([]byte(strings.Repeat("x", maxLiveLogBytes+100)))
	snapshot = logs.snapshot()
	if len(strings.Join(snapshot.lines, "\n")) > maxLiveLogBytes {
		t.Fatal("unbounded partial line")
	}
}
func TestExecStreamIsDuplexResizableAndStoppedOnClose(t *testing.T) {
	var stopped atomic.Int32
	received := make(chan string, 1)
	resized := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/info":
			fmt.Fprint(w, `{"KiriExecStop":true}`)
		case "/containers/test/exec":
			var spec struct {
				AttachStdin, Tty bool
				Cmd              []string
			}
			json.NewDecoder(r.Body).Decode(&spec)
			if !spec.AttachStdin || !spec.Tty || len(spec.Cmd) != 1 {
				t.Errorf("spec: %+v", spec)
			}
			fmt.Fprint(w, `{"Id":"owned"}`)
		case "/exec/owned/start":
			_, _ = io.Copy(io.Discard, r.Body)
			conn, b, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			defer conn.Close()
			b.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\nready\n")
			b.Flush()
			input := make([]byte, 4)
			_, err = io.ReadFull(b, input)
			if err != nil {
				t.Error(err)
				return
			}
			received <- string(input)
			_, _ = io.Copy(io.Discard, b)
		case "/exec/owned/resize":
			resized <- r.URL.RawQuery
			w.WriteHeader(204)
		case "/kiri/exec/owned/stop":
			stopped.Add(1)
			w.WriteHeader(204)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	stream, err := streamClient(server).openExec(t.Context(), "test", []string{"/bin/sh"}, true, true, false)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	ready := make([]byte, 6)
	if _, err = io.ReadFull(stream, ready); err != nil || string(ready) != "ready\n" {
		t.Fatalf("read %q %v", ready, err)
	}
	if _, err = stream.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	if <-received != "ping" {
		t.Fatal("input changed")
	}
	if err = stream.Resize(100, 35); err != nil {
		t.Fatal(err)
	}
	if <-resized != "h=35&w=100" {
		t.Fatal("resize order")
	}
	stream.Close()
	stream.Close()
	<-stream.done
	if stream.cleanupErr != nil || stopped.Load() != 1 {
		t.Fatalf("cleanup %v calls=%d", stream.cleanupErr, stopped.Load())
	}
}
func TestExecStreamRejectsOldEngineBeforeCreatingAShell(t *testing.T) {
	var creates atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/info" {
			creates.Add(1)
		}
		fmt.Fprint(w, `{}`)
	}))
	defer server.Close()
	_, err := streamClient(server).openExec(t.Context(), "test", []string{"sh"}, true, true, false)
	if err == nil || creates.Load() != 0 {
		t.Fatalf("old engine accepted: %v", err)
	}
}
func TestHostTerminalPreservesStateAndHasARealPTY(t *testing.T) {
	if os.Getenv("KIRI_TERMINAL_TEST") != "1" {
		t.Skip("set KIRI_TERMINAL_TEST=1 for the native terminal library and real PTY check")
	}
	if runtime.GOOS == "windows" {
		t.Skip("Unix shell; Windows ConPTY requires platform verification")
	}
	var session *terminalSession
	session = newTerminalSession(t.Context(), "test", "", 0, func() {}, func(ctx context.Context) (*terminal.Terminal, *execStream, error) {
		term, err := terminal.New(terminal.Options{Command: []string{"/bin/sh"}, Dir: t.TempDir(), Env: []string{"PS1=prompt> ", "ENV="}})
		return term, nil, err
	})
	defer session.close()
	waitStream(t, func() bool {
		term, _, err, _ := session.snapshot()
		if err != "" {
			t.Fatal(err)
		}
		return term != nil
	})
	term, _, _, _ := session.snapshot()
	defer func() {
		if t.Failed() {
			t.Logf("terminal transcript: %q", term.Text())
		}
	}()
	tt := ui.NewTester(func(c *ui.Context) { terminal.View(c, term).Fill().AutoFocus() }, 640, 400)
	tt.Type("export KEPT=value; printf 'tty-%s\\n' $(test -t 0 && echo yes)")
	tt.Key(0, ui.KeyEnter)
	waitDesktop(t, tt, func() bool { return strings.Contains(term.Text(), "tty-yes") })
	tt.Type("printf 'saved-%s\\n' \"$KEPT\"")
	tt.Key(0, ui.KeyEnter)
	waitDesktop(t, tt, func() bool { return strings.Contains(term.Text(), "saved-value") })
	tt.Type("sh -c 'printf \"ready-%s\\n\" sleeping; exec sleep 60'")
	tt.Key(0, ui.KeyEnter)
	waitDesktop(t, tt, func() bool { return strings.Contains(term.Text(), "ready-sleeping") })
	prompts := strings.Count(term.Text(), "prompt>")
	tt.Key(ui.Ctrl, ui.KeyC)
	waitDesktop(t, tt, func() bool { return strings.Count(term.Text(), "prompt>") > prompts })
	tt.Type("printf 'after-%s\\n' interrupt")
	tt.Key(0, ui.KeyEnter)
	waitDesktop(t, tt, func() bool { return strings.Contains(term.Text(), "after-interrupt") })
	session.close()
	select {
	case <-session.done:
	case <-time.After(4 * time.Second):
		t.Fatal("shell not reaped")
	}
}

func TestSwitchingLogTargetsCancelsTheOldSubscription(t *testing.T) {
	f, s := newFakeEngine(t)
	closedAPI, closedWeb := make(chan struct{}), make(chan struct{})
	intercept(f, func(w http.ResponseWriter, r *http.Request) bool {
		var marker string
		var done chan struct{}
		switch r.URL.Path {
		case "/containers/c-api/logs":
			marker, done = "only-api", closedAPI
		case "/containers/c-web/logs":
			marker, done = "only-web", closedWeb
		default:
			return false
		}
		if r.Header.Get("X-Kiri-Background") != "1" {
			t.Error("foreground log subscription")
		}
		if r.URL.Query().Get("follow") != "1" {
			t.Error("finite log poll remained")
		}
		fmt.Fprintln(w, marker)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(done)
		return true
	})
	s.selectRow("c-api")
	tt := ui.NewTester(s.view, 1180, 760)
	waitDesktop(t, tt, func() bool { return tt.HasText("only-api") })
	click(t, tt, "Details for shop-web-1")
	waitDesktop(t, tt, func() bool { return tt.HasText("only-web") })
	if tt.HasText("only-api") {
		t.Fatal("previous container's output remained visible")
	}
	select {
	case <-closedAPI:
	case <-time.After(time.Second):
		t.Fatal("previous stream still subscribed")
	}
	click(t, tt, "Info")
	select {
	case <-closedWeb:
	case <-time.After(time.Second):
		t.Fatal("hidden logs still subscribed")
	}
}
