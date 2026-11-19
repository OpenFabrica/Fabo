package main

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/OpenFabrica/Fabo/engine/internal/guestwire"
)

// Dynamic memory, as upstream Kiri does it: the VM boots with all of its
// memory, and the balloon keeps it at what the guest's processes hold plus
// headroom, giving the rest back to macOS. A memory stall in the guest grows
// it at once. The page cache is not counted as held: it is what shrinks.
const (
	balloonHeadroom  = 256 << 20
	balloonFloor     = 256 << 20
	balloonStep      = 256 << 20 // most memory taken back per sample
	balloonGrow      = 1 << 30   // given back at once on a stall
	balloonMinChange = 64 << 20  // smaller moves are not worth a request
	balloonInterval  = time.Second
)

// balloon drives one VM's memory balloon for as long as that VM runs.
type balloon struct {
	// set asks the VM to run in the given bytes; false once it is gone.
	set func(bytes uint64) bool
	// stats samples the guest; set once the engine for this VM exists.
	stats    func(context.Context) (guestwire.Stats, error)
	ceiling  uint64
	pressure chan struct{}
	target   atomic.Uint64
	used     atomic.Uint64
}

// newBalloon starts at the VM's full memory, where the balloon device is
// at boot.
func newBalloon(set func(uint64) bool, ceiling uint64) *balloon {
	b := &balloon{set: set, ceiling: ceiling, pressure: make(chan struct{}, 1)}
	b.target.Store(ceiling)
	return b
}

// notifyPressure is called on the guest session's reader for each stall
// notice. It never blocks: one pending notice is enough.
func (b *balloon) notifyPressure() {
	select {
	case b.pressure <- struct{}{}:
	default:
	}
}

// nextBalloonTarget is the target after a sample: what the guest holds
// plus headroom, within the floor and the ceiling. It shrinks at most
// balloonStep at a time, so the guest's caches go gradually, grows at
// once, and does not move for less than balloonMinChange.
func nextBalloonTarget(target, used, ceiling uint64) uint64 {
	want := min(max(used+balloonHeadroom, balloonFloor), ceiling)
	next := want
	if want < target {
		next = max(want, target-min(balloonStep, target))
	}
	next &^= 1<<20 - 1 // the framework takes whole MiB
	if next > target && next-target < balloonMinChange || next < target && target-next < balloonMinChange {
		return target
	}
	return next
}

// grownBalloonTarget is the target after a memory stall in the guest.
func grownBalloonTarget(target, ceiling uint64) uint64 {
	return min(target+balloonGrow, ceiling)
}

// run samples the guest every interval and moves the target, and grows it
// on each stall notice, until ctx ends with the VM.
func (b *balloon) run(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-b.pressure:
			b.apply(grownBalloonTarget(b.target.Load(), b.ceiling))
		case <-ticker.C:
			// A sample waits behind any guest request in flight; a slow
			// one is skipped rather than queued.
			sampleCtx, cancel := context.WithTimeout(ctx, interval)
			stats, err := b.stats(sampleCtx)
			cancel()
			if err != nil || stats.MemoryUsedKiB == 0 {
				continue
			}
			used := stats.MemoryUsedKiB << 10
			b.used.Store(used)
			b.apply(nextBalloonTarget(b.target.Load(), used, b.ceiling))
		}
	}
}

// apply sets a new target, keeping the old one if the VM refused it.
func (b *balloon) apply(next uint64) {
	if next != b.target.Load() && b.set(next) {
		b.target.Store(next)
	}
}
