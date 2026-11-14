package guestagent

import (
	"os"
	"strings"
	"testing"
	"time"
)

// The status pipe tells a finished setup only by setupReady: a pipe that
// closes without it, as when the child is killed, is a failure.
func TestReadSetup(t *testing.T) {
	for _, tt := range []struct {
		name, content, message string
		failed                 bool
	}{
		{"exec", string([]byte{setupReady}), "", false},
		{"command missing", string([]byte{setupReady}) + "exec: \"x\": executable file not found in $PATH\n", "exec: \"x\": executable file not found in $PATH", false},
		{"killed", "", "", true},
		{"setup error", "mount overlay: invalid argument\n", "mount overlay: invalid argument", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r, w, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			if _, err := w.WriteString(tt.content); err != nil {
				t.Fatal(err)
			}
			_ = w.Close()
			child := &childProcess{setup: r, setupDone: make(chan struct{})}
			reported := false
			message := child.readSetup(func() { reported = true })
			if done, ok := child.settled(); !done || ok == tt.failed || message != tt.message || reported == tt.failed {
				t.Fatalf("settled %v, ok %v, reported %v, message %q", done, ok, reported, message)
			}
		})
	}
}

// lockSettled waits for a child's setup without holding the runtime lock and
// returns with the lock held, refuses a failed setup with the lock released,
// and follows a restart that replaced the child meanwhile.
func TestLockSettled(t *testing.T) {
	const id = "0123456789ab"
	settle := func(r *Runtime, child *childProcess, ready func()) (*childProcess, error) {
		t.Helper()
		r.children[id] = child
		type result struct {
			child *childProcess
			err   error
		}
		got := make(chan result, 1)
		go func() {
			c, err := r.lockSettled(id)
			got <- result{c, err}
		}()
		// While it waits, the lock stays free for everyone else.
		time.Sleep(20 * time.Millisecond)
		if !r.mu.TryLock() {
			t.Fatal("runtime lock held while waiting for setup")
		}
		ready()
		r.mu.Unlock()
		select {
		case res := <-got:
			return res.child, res.err
		case <-time.After(3 * time.Second):
			t.Fatal("lockSettled did not return")
			return nil, nil
		}
	}

	t.Run("settled", func(t *testing.T) {
		r := &Runtime{children: map[string]*childProcess{}}
		child := &childProcess{setupDone: make(chan struct{})}
		got, err := settle(r, child, func() { close(child.setupDone) })
		if err != nil || got != child {
			t.Fatalf("got %p, %v", got, err)
		}
		if r.mu.TryLock() {
			t.Fatal("lockSettled returned without the lock")
		}
		r.mu.Unlock()
	})
	t.Run("failed", func(t *testing.T) {
		r := &Runtime{children: map[string]*childProcess{}}
		child := &childProcess{setupDone: make(chan struct{})}
		got, err := settle(r, child, func() { child.setupFailed = true; close(child.setupDone) })
		if err == nil || got != nil {
			t.Fatalf("failed setup admitted: %p, %v", got, err)
		}
		if !r.mu.TryLock() {
			t.Fatal("lock kept after a refused setup")
		}
		r.mu.Unlock()
	})
	t.Run("restarted", func(t *testing.T) {
		r := &Runtime{children: map[string]*childProcess{}}
		old, restarted := &childProcess{setupDone: make(chan struct{})}, &childProcess{}
		got, err := settle(r, old, func() {
			r.children[id] = restarted
			old.setupFailed = true
			close(old.setupDone)
		})
		if err != nil || got != restarted {
			t.Fatalf("got %p, %v; want the restarted child", got, err)
		}
		r.mu.Unlock()
	})
	t.Run("not running", func(t *testing.T) {
		r := &Runtime{children: map[string]*childProcess{}}
		got, err := r.lockSettled(id)
		if err != nil || got != nil {
			t.Fatalf("got %p, %v", got, err)
		}
		r.mu.Unlock()
	})
}

// Queries on the serial control channel refuse a container that is still
// setting up instead of waiting for it.
func TestRequireSettled(t *testing.T) {
	starting := &childProcess{setupDone: make(chan struct{})}
	if err := starting.requireSettled("c"); err == nil || !strings.Contains(err.Error(), "still starting") {
		t.Fatalf("starting: %v", err)
	}
	failed := &childProcess{setupDone: make(chan struct{}), setupFailed: true}
	close(failed.setupDone)
	if err := failed.requireSettled("c"); err == nil || !strings.Contains(err.Error(), "failed to start") {
		t.Fatalf("failed: %v", err)
	}
	ready := &childProcess{setupDone: make(chan struct{})}
	close(ready.setupDone)
	if err := ready.requireSettled("c"); err != nil {
		t.Fatalf("ready: %v", err)
	}
	if err := (&childProcess{}).requireSettled("c"); err != nil {
		t.Fatalf("started after exec: %v", err)
	}
}
