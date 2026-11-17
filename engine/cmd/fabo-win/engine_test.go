package main

import (
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestEngineControlSerializes: starts, stops and restarts from every
// caller never overlap, and the watchdog leaves a stopped engine alone.
func TestEngineControlSerializes(t *testing.T) {
	var running, overlaps, starts atomic.Int32
	var up atomic.Bool
	work := func(after bool) func() error {
		return func() error {
			if running.Add(1) > 1 {
				overlaps.Add(1)
			}
			time.Sleep(2 * time.Millisecond)
			up.Store(after)
			running.Add(-1)
			return nil
		}
	}
	e := &engineControl{alive: up.Load, stop: work(false), restart: work(true), shutdown: work(false)}
	startWork := work(true)
	e.start = func() error { starts.Add(1); return startWork() }
	var wg sync.WaitGroup
	for i := range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			switch i % 5 {
			case 0:
				e.ensure(true)
			case 1:
				e.userStart()
			case 2:
				e.userStop()
			case 3:
				e.userRestart()
			case 4:
				e.userRestartWSL()
			}
		}()
	}
	wg.Wait()
	if overlaps.Load() != 0 {
		t.Fatalf("%d overlapping engine operations", overlaps.Load())
	}
	e.userStop()
	before := starts.Load()
	if err := e.ensure(true); err != nil || starts.Load() != before || up.Load() {
		t.Fatal("the watchdog started an engine the user stopped")
	}
	if e.userStart(); !up.Load() || e.userStopped.Load() || e.current() != "" {
		t.Fatal("Start did not start the engine")
	}
	if e.ensure(false); starts.Load() != before+1 {
		t.Fatal("an engine that answers was started again")
	}
}

// TestRestartWSLStartsTheEngineAgain: the restart shuts WSL down, then
// starts the engine even after the user had stopped it; a failed shutdown
// starts nothing.
func TestRestartWSLStartsTheEngineAgain(t *testing.T) {
	var steps []string
	var up bool
	e := &engineControl{
		alive:    func() bool { return up },
		start:    func() error { steps = append(steps, "start"); up = true; return nil },
		stop:     func() error { up = false; return nil },
		shutdown: func() error { steps = append(steps, "shutdown"); up = false; return nil },
	}
	e.userStop()
	if err := e.userRestartWSL(); err != nil || !up || e.userStopped.Load() || strings.Join(steps, ",") != "shutdown,start" {
		t.Fatal(err, up, steps)
	}
	steps = nil
	e.shutdown = func() error { return errors.New("wsl --shutdown failed") }
	if err := e.userRestartWSL(); err == nil || len(steps) != 0 {
		t.Fatal(err, steps)
	}
}
