//go:build linux || darwin

package localengine

import (
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

	"github.com/OpenFabrica/Fabo/engine/internal/guestwire"
)

// Output and exit can arrive before the HTTP upgrade finishes for a short exec.
// The docker CLI asks to upgrade and gets 101; docker-java (Testcontainers)
// does not, and gets 200 with the stream as the body, closed after the exit
// code is recorded.
func TestExecKeepsImmediateOutputAndExit(t *testing.T) {
	t.Run("upgrade", func(t *testing.T) { testExecKeepsImmediateOutputAndExit(t, true) })
	t.Run("plain", func(t *testing.T) { testExecKeepsImmediateOutputAndExit(t, false) })
}

// testExecKeepsImmediateOutputAndExit runs one short exec, with or without
// the client's Upgrade header, and checks the reply, its stream and the exit.
func testExecKeepsImmediateOutputAndExit(t *testing.T, upgrade bool) {
	c := &container{id: "target", state: "running"}
	c.nextAttempt()
	e := &Engine{ctx: t.Context(), containers: map[string]*container{c.id: c}}
	host, guest := net.Pipe()
	defer guest.Close()
	e.session = guestwire.NewSession(host, e.onEvent)
	defer e.session.Close()
	_ = guest.SetDeadline(time.Now().Add(3 * time.Second))
	server := httptest.NewServer(e.Handler())
	defer server.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	request := func(method, path, body string) *http.Response {
		t.Helper()
		req, err := http.NewRequestWithContext(ctx, method, server.URL+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		if upgrade && strings.HasSuffix(path, "/start") {
			req.Header.Set("Connection", "Upgrade")
			req.Header.Set("Upgrade", "tcp")
		}
		res, err := server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = res.Body.Close() })
		return res
	}

	res := request(http.MethodPost, "/containers/target/exec", `{"Cmd":["/bin/test"],"AttachStdout":true,"Privileged":true}`)
	var created struct{ ID string }
	if err := json.NewDecoder(res.Body).Decode(&created); res.StatusCode != http.StatusCreated || err != nil {
		t.Fatalf("create: %s, %v", res.Status, err)
	}
	res.Body.Close()
	peer := make(chan error, 1)
	go func() {
		frame, err := guestwire.ReadFrame(guest, nil)
		if err == nil {
			spec, decodeErr := guestwire.DecodeExec(frame)
			if frame.Kind != guestwire.ExecMessage || decodeErr != nil || spec.ID != created.ID || spec.ContainerID != c.id || spec.Flags&guestwire.RunPrivileged == 0 {
				err = fmt.Errorf("unexpected exec request: %+v, %v", spec, decodeErr)
			}
		}
		credit := make(chan error, 1)
		if err == nil {
			go func() {
				out, err := guestwire.ReadFrame(guest, nil)
				if err == nil {
					id, count, decodeErr := guestwire.DecodeExecOutputCredit(out)
					if out.Kind != guestwire.ExecOutputCreditMessage || decodeErr != nil || id != created.ID || count != 1 {
						err = fmt.Errorf("unexpected output credit: %q %d %v", id, count, decodeErr)
					}
				}
				credit <- err
			}()
		}
		for _, event := range []guestwire.RunEvent{
			{Kind: guestwire.StartedMessage, ID: created.ID, PID: 9},
			{Kind: guestwire.OutputMessage, ID: created.ID, Stream: 1, Data: []byte("done")},
			{Kind: guestwire.ExitedMessage, ID: created.ID, Exit: guestwire.Exit{Code: 37}},
		} {
			if err != nil {
				break
			}
			out, encodeErr := event.Frame()
			if encodeErr != nil {
				err = encodeErr
				break
			}
			err = guestwire.WriteFrame(guest, out)
		}
		if err == nil {
			err = <-credit
		}
		peer <- err
	}()
	res = request(http.MethodPost, "/exec/"+created.ID+"/start", `{"Detach":false}`)
	want := http.StatusOK
	if upgrade {
		want = http.StatusSwitchingProtocols
	}
	if res.StatusCode != want || res.Header.Get("Content-Type") != "application/vnd.docker.raw-stream" {
		t.Fatalf("start: %s %v", res.Status, res.Header)
	}
	if got := res.Header.Get("Upgrade"); (got == "tcp") != upgrade {
		t.Fatalf("start Upgrade header %q", got)
	}
	body, err := io.ReadAll(res.Body)
	res.Body.Close()
	if err != nil || string(body) != "\x01\x00\x00\x00\x00\x00\x00\x04done" {
		t.Fatalf("exec output: %q, %v", body, err)
	}
	if err := <-peer; err != nil {
		t.Fatal(err)
	}
	res = request(http.MethodGet, "/exec/"+created.ID+"/json", "")
	var inspected struct {
		Running  bool
		ExitCode *int32
	}
	if err := json.NewDecoder(res.Body).Decode(&inspected); err != nil || inspected.Running || inspected.ExitCode == nil || *inspected.ExitCode != 37 {
		t.Fatalf("inspect: %+v, %v", inspected, err)
	}
}
