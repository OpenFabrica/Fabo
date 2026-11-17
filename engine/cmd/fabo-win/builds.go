package main

import (
	"errors"
	"os"
	"sync"
	"time"

	"github.com/OpenFabrica/Fabo/engine/internal/buildwith"
)

// Which builder plain `docker build` uses on this PC, Docker's or Fabo's
// (internal/buildwith, shared with the Mac). fabo-win applies the choice
// when it starts and when setup finishes, the window changes it through
// the control pipe, and what "Build with Fabo" took from docker's config is
// given back when fabo-win quits, when its program is gone, and on
// uninstall (`fabo-win --uninstall-builds`, from fabo.ps1). Upstream
// Kiri's kiri-win main.rs and engine.rs.

// builds runs every build-with change one at a time: the window, startup,
// setup and quitting reach it from different goroutines, and each change
// reads and rewrites the same files.
type builds struct {
	mu sync.Mutex
	// places is where docker's files and build.json are.
	places func() (buildwith.Places, error)
	// other names the engine besides Fabo on this PC, "" when none.
	other func() string
	logf  func(format string, args ...any)
	// over: fabo-win is quitting and has handed the builds back; a late
	// apply must not take them again behind it.
	over bool
}

// apply makes the saved choice so. Fabo is never flavored on Windows, so
// with nothing chosen and no other engine it takes the builds by itself.
func (b *builds) apply() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.over {
		return
	}
	p, err := b.places()
	if err == nil {
		err = p.Apply(b.other(), true)
	}
	if err != nil {
		b.logf("docker build: could not set which builder it uses (%v)", err)
	}
}

// choose is the window's choice, made so now.
func (b *builds) choose(with string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.over {
		return errors.New("Fabo is quitting")
	}
	p, err := b.places()
	if err != nil {
		return err
	}
	return p.Choose(with)
}

// status is GET /buildwith.
func (b *builds) status() buildwith.Status {
	b.mu.Lock()
	defer b.mu.Unlock()
	p, err := b.places()
	if err != nil {
		return buildwith.Status{Error: err.Error()}
	}
	return p.Status(b.other(), true)
}

// giveBack puts docker's builder back as it was before Fabo took it,
// keeping the choice: fabo-win quitting or its program gone leaves docker
// build pointing at a builder nobody serves otherwise. Nothing is taken
// again by this process; the next start does that.
//
// onlyTaken (quitting) gives back only while every build still goes to
// Fabo: once the user has picked a builder of their own (`docker buildx
// use`), the snapshot stays, so the next start does not take the builds
// again over that choice. A program that is gone gives back regardless,
// as upstream does.
func (b *builds) giveBack(why string, onlyTaken bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.over = true
	p, err := b.places()
	if err == nil && (!onlyTaken || p.Taken()) {
		err = p.GiveBack()
	}
	if err != nil {
		b.logf("docker build not handed back (%s): %v", why, err)
	}
}

// uninstallBuilds is `fabo-win --uninstall-builds`: docker build's own
// builder back, the context's copy of Fabo's builder and build.json gone.
// Its exit code tells fabo.ps1 whether that worked.
func uninstallBuilds(places func() (buildwith.Places, error), logf func(string, ...any)) int {
	p, err := places()
	if err == nil {
		err = p.Uninstall()
	}
	if err != nil {
		logf("uninstall: docker build's builder: %v", err)
		return 1
	}
	return 0
}

// watchProgram calls onGone once, on its own goroutine, when path has been
// missing on two looks in a row: Windows will not delete a running
// program, but its folder can be renamed or moved around it. An update
// swaps the file in well under two looks. Upstream's appwatch.rs.
func watchProgram(path string, every time.Duration, onGone func()) {
	go func() {
		for missing := 0; missing < 2; {
			time.Sleep(every)
			if _, err := os.Stat(path); err == nil {
				missing = 0
			} else {
				missing++
			}
		}
		onGone()
	}()
}
