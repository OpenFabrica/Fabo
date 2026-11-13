//go:build linux || darwin

package localengine

import (
	"testing"
	"time"
)

func TestIdleShutdownProtectsWorkAndAllowsOnlyManagedBuilders(t *testing.T) {
	managed := &container{
		name: defaultManagedBuilder, state: "running",
		labels: map[string]string{"dev.kiri.builder": "1"},
	}
	job := &container{name: "app", state: "running", labels: managed.labels}
	e := &Engine{containers: map[string]*container{"builder": managed, "app": job}}
	if e.PrepareIdleShutdown() {
		t.Fatal("ordinary container with a builder label did not keep the guest awake")
	}
	job.state = "exited"
	job.restartTimer = time.NewTimer(time.Hour)
	if e.PrepareIdleShutdown() {
		t.Fatal("scheduled restart did not keep the guest awake")
	}
	job.restartTimer.Stop()
	job.restartTimer = nil
	if e.PrepareIdleShutdown() {
		t.Fatal("builder was stopped before its idle threshold")
	}
	managed.paused = true
	e.execs = map[string]*execProcess{"x": {running: true}}
	if e.PrepareIdleShutdown() {
		t.Fatal("active exec did not keep the guest awake")
	}
	delete(e.execs, "x")
	if !e.PrepareIdleShutdown() || !e.closed {
		t.Fatal("idle guest with only its managed builder was not reserved for shutdown")
	}
}
