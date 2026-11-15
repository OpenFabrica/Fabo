//go:build linux || (darwin && cgo)

package main

import (
	"context"
	"errors"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/OpenFabrica/Fabo/engine/internal/localengine"
)

// TestShareWakesSleepingVM checks a share link's first visitor wakes a
// sleeping VM before dialing, and that a manually stopped VM stays stopped.
func TestShareWakesSleepingVM(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	state, err := localengine.OpenState(dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	saved := shareRestartWait
	shareRestartWait = 300 * time.Millisecond
	t.Cleanup(func() { shareRestartWait = saved })
	s := &supervisor{ctx: ctx, state: state, vm: &vmConfiguration{settings: localengine.VMSettings{CPUs: 2, MemoryMiB: 2048}}}
	wakes := 0
	s.wakeForShare = func() error {
		wakes++
		// A piped guest that never speaks keeps the engine running.
		guest, peer := net.Pipe()
		t.Cleanup(func() { peer.Close() })
		engine, err := localengine.New(ctx, guest, nil, state, localengine.VMOptions{Events: &s.events})
		if err != nil {
			return err
		}
		s.mu.Lock()
		s.current = &engineInstance{engine: engine, handler: engine.Handler()}
		s.active = true
		s.mu.Unlock()
		return nil
	}
	before := time.Now()
	// The VM sleeps: dialing wakes it, then looks the container up, waiting
	// a while for a restart; there is none here, so it gives up.
	if _, err := s.dialShare(ctx, "web"); err == nil || errors.Is(err, errManuallyStopped) || wakes != 1 {
		t.Fatalf("dial after wake: %v, %d wakes", err, wakes)
	}
	if waited := time.Since(before); waited < shareRestartWait {
		t.Fatalf("gave up after %v without waiting for a restart", waited)
	}
	if s.lastActivity.Before(before) {
		t.Fatal("a visitor should count as activity")
	}
	// Awake now: no second boot.
	if _, err := s.resolveShareContainer(ctx, "web"); err == nil || !strings.Contains(err.Error(), "no port or container named web") || wakes != 1 {
		t.Fatalf("resolve: %v, %d wakes", err, wakes)
	}
	// A VM the user stopped is not started by a visitor.
	s.mu.Lock()
	s.active = false
	s.vm.settings.Stopped = true
	s.mu.Unlock()
	if _, err := s.dialShare(ctx, "web"); !errors.Is(err, errManuallyStopped) || wakes != 1 {
		t.Fatalf("stopped VM: %v, %d wakes", err, wakes)
	}
}
