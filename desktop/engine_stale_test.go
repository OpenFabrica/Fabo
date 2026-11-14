package main

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/egoist/mygo/ui"
)

// staleDesktop is a window of version `version` on the fake engine, which
// runs 0.4.0, with a launcher that records its calls.
func staleDesktop(t *testing.T, version string) (*desktop, *[][]string) {
	t.Helper()
	saved := release
	release = version
	t.Cleanup(func() { release = saved })
	_, s := newFakeEngine(t)
	calls := &[][]string{}
	s.launcher = func(_ context.Context, args ...string) ([]byte, error) {
		*calls = append(*calls, args)
		return nil, nil
	}
	return s, calls
}

// restarts counts the launcher calls that restart the engine; the window
// also asks the launcher other things as it opens.
func restarts(calls [][]string) int {
	n := 0
	for _, call := range calls {
		if strings.Join(call, " ") == "engine restart" {
			n++
		}
	}
	return n
}

// A window of another version than its engine asks to restart the engine,
// and the restart goes through `fabo engine restart`.
func TestStaleEngineStripRestarts(t *testing.T) {
	s, calls := staleDesktop(t, "0.5.0")
	tt := ui.NewTester(s.view, 1180, 900)
	waitDesktop(t, tt, func() bool { return tt.HasText("The engine still runs 0.4.0,") })
	if !slices.ContainsFunc(tt.Texts(), func(text string) bool { return strings.Contains(text, "not Fabo 0.5.0.") }) {
		t.Fatalf("strip does not name the window's version: %q", tt.Texts())
	}
	click(t, tt, "Restart engine")
	if restarts(*calls) != 1 {
		t.Fatalf("launcher calls %v", *calls)
	}
	// The view turns the notice into a toast.
	waitDesktop(t, tt, func() bool { return tt.HasText("Engine restarted") })
}

// Later puts the strip off for this window; the card still shows the
// engine's version.
func TestStaleEngineStripLater(t *testing.T) {
	s, calls := staleDesktop(t, "0.5.0")
	tt := ui.NewTester(s.view, 1180, 900)
	waitDesktop(t, tt, func() bool { return tt.HasText("The engine still runs 0.4.0,") })
	click(t, tt, "Later")
	tt.Frame()
	if tt.HasText("The engine still runs 0.4.0,") || restarts(*calls) != 0 {
		t.Fatalf("strip after Later: %q, calls %v", tt.Texts(), *calls)
	}
	if !tt.HasText("v0.4.0") {
		t.Fatal("card lost the engine's version")
	}
}

// The same version, or a development window, asks nothing.
func TestStaleEngineStripQuiet(t *testing.T) {
	for _, version := range []string{"0.4.0", "0.0.0-dev"} {
		s, _ := staleDesktop(t, version)
		tt := ui.NewTester(s.view, 1180, 900)
		waitDesktop(t, tt, func() bool { return tt.HasText("v0.4.0") })
		if tt.HasText("The engine still runs 0.4.0,") {
			t.Fatalf("window %s asked to restart an engine of 0.4.0", version)
		}
	}
}
