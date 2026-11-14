package main

import (
	"strings"
	"testing"

	"github.com/egoist/mygo/ui"
)

// Settings → About names the host-guest protocol on the engine's row.
func TestAboutShowsGuestProtocol(t *testing.T) {
	_, s := newFakeEngine(t)
	s.navigate(settingsPage)
	s.settingsTab = indexSettings("about")
	tt := ui.NewTester(s.view, 1180, 900)
	want := "Fabo 0.4.0 · Docker API 1.47 · protocol 0.5"
	waitDesktop(t, tt, func() bool {
		for _, text := range tt.Texts() {
			if strings.Contains(text, want) {
				return true
			}
		}
		return false
	})
}

// The sidebar's engine card shows the engine's version as a line, not only
// in its tooltip.
func TestEngineCardShowsVersion(t *testing.T) {
	_, s := newFakeEngine(t)
	tt := ui.NewTester(s.view, 1180, 900)
	waitDesktop(t, tt, func() bool { return tt.HasText("v0.4.0") })
	if !tt.HasText("Version") {
		t.Fatalf("no Version label in %q", tt.Texts())
	}
}
