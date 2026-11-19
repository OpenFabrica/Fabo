package main

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/OpenFabrica/Fabo/engine/internal/guestwire"
)

const mib = 1 << 20

// The target follows what the guest holds plus headroom: down a step at a
// time, up at once, within the floor and the ceiling, in whole MiB, and
// not for a small move.
func TestNextBalloonTarget(t *testing.T) {
	ceiling := uint64(8192 * mib)
	for _, tc := range []struct {
		name               string
		target, used, want uint64
	}{
		{"boot shrinks a step", ceiling, 700 * mib, ceiling - 256*mib},
		{"close to want stops there", 1200 * mib, 700 * mib, 956 * mib},
		{"at want stays", 956 * mib, 700 * mib, 956 * mib},
		{"small growth waits", 956 * mib, 740 * mib, 956 * mib},
		{"growth is at once", 956 * mib, 3000 * mib, 3256 * mib},
		{"growth stops at the ceiling", 4096 * mib, 8000 * mib, ceiling},
		{"idle guest goes to the floor", 400 * mib, 0, 256 * mib},
		{"too small a shrink waits", 300 * mib, 0, 300 * mib},
		{"never below the floor", 256 * mib, 0, 256 * mib},
		{"whole MiB", 1400 * mib, 1000*mib + 12345, 1256 * mib},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := nextBalloonTarget(tc.target, tc.used, ceiling); got != tc.want {
				t.Fatalf("nextBalloonTarget(%d MiB, %d B) = %d MiB, want %d MiB", tc.target/mib, tc.used, got/mib, tc.want/mib)
			}
		})
	}
	if got := grownBalloonTarget(1000*mib, ceiling); got != 2024*mib {
		t.Fatalf("grow from 1000 MiB = %d MiB", got/mib)
	}
	if got := grownBalloonTarget(ceiling-100*mib, ceiling); got != ceiling {
		t.Fatalf("grow near the ceiling = %d MiB", got/mib)
	}
}

// The loop samples the guest and moves the target, grows it on a stall
// notice, skips failed samples, keeps the old target when the VM refuses a
// new one, and ends with its context.
func TestBalloonRun(t *testing.T) {
	var mu sync.Mutex
	var sets []uint64
	refuse := false
	set := func(bytes uint64) bool {
		mu.Lock()
		defer mu.Unlock()
		if refuse {
			return false
		}
		sets = append(sets, bytes)
		return true
	}
	b := newBalloon(set, 2048*mib)
	samples := make(chan guestwire.Stats)
	b.stats = func(ctx context.Context) (guestwire.Stats, error) {
		select {
		case s, ok := <-samples:
			if !ok {
				return guestwire.Stats{}, errors.New("no guest")
			}
			return s, nil
		case <-ctx.Done():
			return guestwire.Stats{}, ctx.Err()
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { b.run(ctx, 5*time.Millisecond); close(done) }()
	waitTarget := func(want uint64) {
		t.Helper()
		deadline := time.Now().Add(2 * time.Second)
		for b.target.Load() != want {
			if time.Now().After(deadline) {
				t.Fatalf("target %d MiB, want %d MiB", b.target.Load()/mib, want/mib)
			}
			time.Sleep(time.Millisecond)
		}
	}
	samples <- guestwire.Stats{MemoryUsedKiB: 500 << 10}
	waitTarget(1792 * mib)
	if b.used.Load() != 500*mib {
		t.Fatalf("used %d", b.used.Load())
	}
	b.notifyPressure()
	b.notifyPressure() // a second pending notice is dropped, not queued
	waitTarget(2048 * mib)
	mu.Lock()
	refuse = true
	mu.Unlock()
	samples <- guestwire.Stats{MemoryUsedKiB: 500 << 10}
	time.Sleep(20 * time.Millisecond)
	if b.target.Load() != 2048*mib {
		t.Fatalf("a refused target was kept: %d MiB", b.target.Load()/mib)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("run did not end with its context")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(sets) != 2 || sets[0] != 1792*mib || sets[1] != 2048*mib {
		t.Fatalf("targets set: %v", sets)
	}
}
