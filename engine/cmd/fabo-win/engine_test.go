package main

import (
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
	e := &engineControl{alive: up.Load, stop: work(false), restart: work(true)}
	startWork := work(true)
	e.start = func() error { starts.Add(1); return startWork() }
	var wg sync.WaitGroup
	for i := range 40 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			switch i % 4 {
			case 0:
				e.ensure(true)
			case 1:
				e.userStart()
			case 2:
				e.userStop()
			case 3:
				e.userRestart()
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
