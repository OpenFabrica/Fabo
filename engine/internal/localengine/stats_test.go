//go:build linux || darwin

package localengine

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/OpenFabrica/FabriBay/engine/internal/guestwire"
)

func TestStatsStreamBelongsToOneRun(t *testing.T) {
	c := &container{id: "id", name: "app", state: "running"}
	c.nextAttempt()
	e := &Engine{containers: map[string]*container{c.id: c}, vmMemory: 1024}
	host, guest := net.Pipe()
	defer guest.Close()
	_ = guest.SetDeadline(time.Now().Add(3 * time.Second))
	e.session = guestwire.NewSession(host, e.onEvent)
	defer e.session.Close()
	server := httptest.NewServer(e.Handler())
	defer server.Close()

	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/v1.44/containers/app/stats", nil)
	if err != nil {
		t.Fatal(err)
	}
	type response struct {
		res *http.Response
		err error
	}
	responses := make(chan response, 1)
	go func() {
		res, err := server.Client().Do(req)
		responses <- response{res, err}
	}()
	frame, err := guestwire.ReadFrame(guest, nil)
	if err != nil || frame.Kind != guestwire.StatsQueryMessage {
		t.Fatalf("stats query: %v, %v", frame.Kind, err)
	}
	if id, err := guestwire.DecodeContainerQuery(frame); err != nil || id != c.id {
		t.Fatalf("stats target: %q, %v", id, err)
	}
	sample := guestwire.ContainerStats{OnlineCPUs: 2, MemoryCurrent: 9000, MemoryInactiveFile: 3000, CPUUsageUsec: 1200, PIDs: 2}
	if err := guestwire.WriteFrame(guest, sample.Frame()); err != nil {
		t.Fatal(err)
	}
	result := <-responses
	if result.err != nil {
		t.Fatal(result.err)
	}
	defer result.res.Body.Close()
	if result.res.StatusCode != http.StatusOK {
		t.Fatalf("stats status: %s", result.res.Status)
	}
	decoder := json.NewDecoder(result.res.Body)
	var stats struct {
		Name   string
		Memory struct {
			Usage, Limit uint64
			Stats        map[string]uint64
		} `json:"memory_stats"`
		PIDs struct{ Current uint32 } `json:"pids_stats"`
	}
	if err := decoder.Decode(&stats); err != nil {
		t.Fatal(err)
	}
	if stats.Name != "/app" || stats.Memory.Usage != 9000 || stats.Memory.Limit != 1<<30 ||
		stats.Memory.Stats["inactive_file"] != 3000 || stats.PIDs.Current != 2 {
		t.Fatalf("first sample: %+v", stats)
	}

	e.onEvent(guestwire.RunEvent{Kind: guestwire.ExitedMessage, ID: c.id})
	e.mu.Lock()
	c.state = "running"
	c.nextAttempt()
	e.mu.Unlock()
	if err := decoder.Decode(&stats); err != io.EOF {
		t.Fatalf("old stream after restart: %v, want EOF", err)
	}
}
