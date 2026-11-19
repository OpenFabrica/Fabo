//go:build linux || (darwin && cgo)

package main

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/OpenFabrica/Fabo/engine/internal/localengine"
)

// bootFixture is a supervisor whose guest boot fails while fail is set.
type bootFixture struct {
	s        *supervisor
	attempts atomic.Int32
	fail     atomic.Bool
}

// newBootFixture builds a VM supervisor over a private state directory,
// with retry delays short enough for a test.
func newBootFixture(t *testing.T) *bootFixture {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	state, err := localengine.OpenState(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { state.Close() })
	first, most := bootRetryFirst, bootRetryMax
	bootRetryFirst, bootRetryMax = 20*time.Millisecond, 40*time.Millisecond
	t.Cleanup(func() { bootRetryFirst, bootRetryMax = first, most })
	ctx, cancel := context.WithCancel(context.Background())
	f := &bootFixture{s: &supervisor{ctx: ctx, state: state, stateDir: dir, vm: &vmConfiguration{
		minCPU: 1, maxCPU: 8, minMemory: 1024, maxMemory: 8192,
		settings: localengine.VMSettings{CPUs: 2, MemoryMiB: 2048},
	}}}
	f.fail.Store(true)
	f.s.startGuest = func(context.Context, guestOptions) (guestConnection, error) {
		f.attempts.Add(1)
		if f.fail.Load() {
			return guestConnection{}, errors.New("the hypervisor is unavailable")
		}
		// A piped guest that never speaks keeps the engine running.
		guest, peer := net.Pipe()
		t.Cleanup(func() { peer.Close() })
		return guestConnection{conn: guest, close: guest.Close}, nil
	}
	t.Cleanup(func() {
		_ = f.s.close(false)
		cancel()
	})
	return f
}

// resources reads /fabo/resources.
func (f *bootFixture) resources(t *testing.T) map[string]any {
	t.Helper()
	w := httptest.NewRecorder()
	f.s.resources(w, httptest.NewRequest(http.MethodGet, "/fabo/resources", nil))
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// waitFor polls cond for up to two seconds.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

// TestBootFailureKeepsServing checks a VM that does not boot leaves the
// daemon up without a guest, says why in /fabo/resources, retries in the
// background, and that Stop ends the retries.
func TestBootFailureKeepsServing(t *testing.T) {
	f := newBootFixture(t)
	if err := f.s.startVM(); err != nil {
		t.Fatalf("a failed boot ended the daemon: %v", err)
	}
	if f.s.current == nil || f.s.isActiveLocked() {
		t.Fatal("no engine without a guest")
	}
	if out := f.resources(t); out["boot_error"] != "the hypervisor is unavailable" || out["awake"] != false {
		t.Fatalf("resources: %v", out)
	}
	// Requests that need no guest are served from the saved state.
	w := httptest.NewRecorder()
	f.s.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1.44/containers/json?all=1", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("list containers: %d %s", w.Code, w.Body)
	}
	waitFor(t, "background retries", func() bool { return f.attempts.Load() >= 3 })

	if err := f.s.sleep(true); err != nil {
		t.Fatal(err)
	}
	stopped := f.attempts.Load()
	time.Sleep(10 * bootRetryMax)
	if got := f.attempts.Load(); got != stopped {
		t.Fatalf("boot tried %d more times after Stop", got-stopped)
	}
	if _, ok := f.resources(t)["boot_error"]; ok {
		t.Fatal("Stop kept the boot error")
	}

	// Start boots it again, explicitly.
	f.fail.Store(false)
	if err := f.s.wake(true); err != nil {
		t.Fatal(err)
	}
	if out := f.resources(t); out["awake"] != true || out["boot_error"] != nil {
		t.Fatalf("after Start: %v", out)
	}
}

// TestBootRetryBootsTheVM checks the background retry brings the VM up once
// the cause goes away, then ends.
func TestBootRetryBootsTheVM(t *testing.T) {
	f := newBootFixture(t)
	if err := f.s.startVM(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "a retry", func() bool { return f.attempts.Load() >= 2 })
	f.fail.Store(false)
	waitFor(t, "the VM to boot", func() bool {
		f.s.mu.Lock()
		defer f.s.mu.Unlock()
		return f.s.isActiveLocked()
	})
	f.s.mu.Lock()
	retrying, bootErr := f.s.cancelBootRetry != nil, f.s.bootErr
	f.s.mu.Unlock()
	if retrying || bootErr != nil {
		t.Fatalf("after boot: retrying %v, error %v", retrying, bootErr)
	}
	booted := f.attempts.Load()
	time.Sleep(10 * bootRetryMax)
	if f.attempts.Load() != booted {
		t.Fatal("retried after the VM booted")
	}
}

// TestFailedStartRestartsRetry checks Start that fails while the VM is meant
// to run starts the background retry over.
func TestFailedStartRestartsRetry(t *testing.T) {
	f := newBootFixture(t)
	if err := f.s.startVM(); err != nil {
		t.Fatal(err)
	}
	f.s.mu.Lock()
	f.s.stopBootRetryLocked()
	f.s.mu.Unlock()
	w := httptest.NewRecorder()
	f.s.manageVM(w, httptest.NewRequest(http.MethodPost, "/fabo/wake", nil), "/fabo/wake")
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("start: %d", w.Code)
	}
	before := f.attempts.Load()
	waitFor(t, "a retry after Start failed", func() bool { return f.attempts.Load() > before })
}

// TestNextBootDelay checks the retry delay doubles from 10 s up to 5 minutes.
func TestNextBootDelay(t *testing.T) {
	delay, seen := bootRetryFirst, []time.Duration{}
	for range 7 {
		seen = append(seen, delay)
		delay = nextBootDelay(delay, bootRetryMax)
	}
	want := []time.Duration{10 * time.Second, 20 * time.Second, 40 * time.Second, 80 * time.Second, 160 * time.Second, 300 * time.Second, 300 * time.Second}
	for i := range want {
		if seen[i] != want[i] {
			t.Fatalf("delays %v, want %v", seen, want)
		}
	}
}
