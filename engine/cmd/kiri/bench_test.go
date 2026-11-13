//go:build linux || darwin

package main

import (
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"
)

func TestBenchOptionsAndPercentiles(t *testing.T) {
	for _, args := range [][]string{{"--runs", "0"}, {"--runs", "-1"}, {"--runs", "x"}, {"--network", "invalid"}, {"--only", "unknown"}, {"--image"}} {
		if _, e := parseBench(args); e == nil {
			t.Fatal(args)
		}
	}
	times := []time.Duration{3, 1, 2, 4}
	if benchPercentile(times, 50) != 3 || benchPercentile(times, 99) != 4 || times[0] != 3 {
		t.Fatal(times)
	}
}
func TestBenchRegistersWaitBeforeStart(t *testing.T) {
	var mu sync.Mutex
	waiting := false
	var exited chan struct{}
	runs := 0
	c := machineTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/containers/create":
			mu.Lock()
			waiting = false
			exited = make(chan struct{})
			mu.Unlock()
			w.WriteHeader(201)
			fmt.Fprint(w, `{"Id":"test"}`)
		case "/containers/test/wait":
			mu.Lock()
			waiting = true
			done := exited
			mu.Unlock()
			w.WriteHeader(200)
			w.(http.Flusher).Flush()
			<-done
			fmt.Fprint(w, `{"StatusCode":0}`)
		case "/containers/test/start":
			mu.Lock()
			if !waiting {
				t.Error("started before wait subscription")
			}
			runs++
			close(exited)
			mu.Unlock()
			w.WriteHeader(204)
		default:
			t.Errorf("unexpected benchmark request %s %s", r.Method, r.URL)
			w.WriteHeader(404)
		}
	})
	times, e := benchAPI(c, benchOptions{runs: 2, image: "alpine", network: "none"})
	if e != nil || len(times) != 2 {
		t.Fatal(times, e)
	}
	mu.Lock()
	defer mu.Unlock()
	if runs != 7 {
		t.Fatal(runs)
	}
}
