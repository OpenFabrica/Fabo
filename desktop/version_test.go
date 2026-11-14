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
