//go:build linux || darwin

package localengine

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"testing"
	"time"

	"github.com/OpenFabrica/FabriBay/engine/internal/guestwire"
)

func TestLogTailCountsAcrossGuestChunks(t *testing.T) {
	var c container
	c.appendLog(record{stream: 1, data: bytes.Repeat([]byte("a"), containerLogLimit)})
	c.appendLog(record{stream: 1, data: []byte("old\nsec")})
	c.appendLog(record{stream: 1, data: []byte("ond\nthird\n")})
	c.appendLog(record{stream: 2, data: []byte("error")})
	for _, tc := range []struct {
		name, stderr, expected string
		tail                   int
	}{
		{"both", "1", "third\nerror", 2},
		{"stdout only", "", "second\nthird\n", 2},
		{"zero", "1", "", 0},
		{"all retained", "1", "old\nsecond\nthird\nerror", -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q := url.Values{"stdout": {"1"}, "stderr": {tc.stderr}}
			tail := selectLogTail(append([]record(nil), c.logs...), q, false, tc.tail)
			var out bytes.Buffer
			for _, entry := range tail {
				out.Write(entry.data)
			}
			if got := out.String(); got != tc.expected {
				t.Fatalf("tail = %q, want %q", got, tc.expected)
			}
		})
	}
}

func TestLogsFollowPreservesLinesAcrossSnapshotAndExit(t *testing.T) {
	c := &container{id: "id", state: "running"}
	c.nextAttempt()
	c.appendLog(record{stream: 1, data: []byte("pre"), at: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)})
	e := &Engine{containers: map[string]*container{c.id: c}}
	server := httptest.NewServer(e.Handler())
	defer server.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/containers/id/logs?stdout=1&stderr=1&follow=1&timestamps=1", nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("follow status: %s", res.Status)
	}
	// Receiving headers establishes that the snapshot and live subscription
	// are both ready. The remainder of the first line comes from the guest.
	e.onEvent(guestwire.RunEvent{Kind: guestwire.OutputMessage, ID: c.id, Stream: 1, Data: []byte("fix\nsecond\n")})
	e.onEvent(guestwire.RunEvent{Kind: guestwire.OutputMessage, ID: c.id, Stream: 2, Data: []byte("error\n")})
	e.onEvent(guestwire.RunEvent{Kind: guestwire.ExitedMessage, ID: c.id})
	var stdout, stderr bytes.Buffer
	for {
		var header [8]byte
		if _, err := io.ReadFull(res.Body, header[:]); err == io.EOF {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		out := &stdout
		if header[0] == 2 {
			out = &stderr
		}
		if _, err := io.CopyN(out, res.Body, int64(binary.BigEndian.Uint32(header[4:]))); err != nil {
			t.Fatal(err)
		}
	}
	stamp := `[0-9T:.\-]+Z `
	if want := regexp.MustCompile(`^2026-01-02T03:04:05\.000000000Z prefix\n` + stamp + `second\n$`); !want.Match(stdout.Bytes()) {
		t.Fatalf("stdout: %q", stdout.String())
	}
	if want := regexp.MustCompile(`^` + stamp + `error\n$`); !want.Match(stderr.Bytes()) {
		t.Fatalf("stderr: %q", stderr.String())
	}
}
