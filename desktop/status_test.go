package main

import (
	"slices"
	"testing"

	"github.com/egoist/mygo/ui"
)

func TestStatusBarNavigationAndEngineState(t *testing.T) {
	s, _ := newImportDesktop(t, "orbstack")
	s.navigate(imagesPage)
	s.prefs.Scale = 1.25
	tt := ui.NewTester(s.view, 860, 520)
	tt.Frame()
	if missing := has(tt, s.appName()+" "+release, "Linux VM · Running", "v0.4.0", "Memory 8.0 GB"); missing != "" {
		t.Fatalf("missing %q in %q", missing, tt.Texts())
	}
	for _, item := range []string{"docker → OrbStack", "Feedback"} {
		rect, found := tt.Find(item)
		if !found || rect.X+rect.W > 860 || rect.Y < 480 || rect.Y+rect.H > 520 {
			t.Fatalf("%q outside the status bar: %v, found %v", item, rect, found)
		}
	}
	click(t, tt, "docker → OrbStack")
	if !s.imp.ask {
		t.Fatal("Docker context did not open the switch dialog")
	}
	s.imp.ask = false
	tt.Frame()
	click(t, tt, "Feedback")
	if got := tt.OpenedURLs(); !slices.Equal(got, []string{issuesURL}) {
		t.Fatalf("feedback opened %v", got)
	}
	click(t, tt, "3/5 running")
	if s.page != containersPage {
		t.Fatalf("container count opened %q", s.page)
	}
	s.info.FaboStopped = true
	tt.Frame()
	if !tt.HasText("Linux VM · Off") || !tt.HasText("v0.4.0") || tt.HasText("3/5 running") {
		t.Fatalf("stopped engine: %q", tt.Texts())
	}
	s.connected = false
	tt.Frame()
	if !tt.HasText("Linux VM · Not connected") || tt.HasText("v0.4.0") {
		t.Fatalf("disconnected engine: %q", tt.Texts())
	}
}
