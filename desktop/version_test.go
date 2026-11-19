package main

import (
	"encoding/json"
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

// The window reads the engine's Fabo release from FaboVersion; an engine
// older than that field (a shared installed engine may be) reported it as
// ServerVersion and Version, which are now the Docker Engine version.
func TestFaboVersionFallsBackForOlderEngines(t *testing.T) {
	var info engineInfo
	if err := json.Unmarshal([]byte(`{"Name":"fabo","ServerVersion":"27.5.1","FaboVersion":"0.6.0"}`), &info); err != nil || info.faboVersion() != "0.6.0" {
		t.Fatalf("current /info: %q, %v", info.faboVersion(), err)
	}
	info = engineInfo{}
	if err := json.Unmarshal([]byte(`{"Name":"fabo","ServerVersion":"0.5.0"}`), &info); err != nil || info.faboVersion() != "0.5.0" {
		t.Fatalf("older /info: %q, %v", info.faboVersion(), err)
	}
	var version versionInfo
	if err := json.Unmarshal([]byte(`{"Version":"27.5.1","FaboVersion":"0.6.0"}`), &version); err != nil || version.fabo() != "0.6.0" {
		t.Fatalf("current /version: %q, %v", version.fabo(), err)
	}
	version = versionInfo{}
	if err := json.Unmarshal([]byte(`{"Version":"0.5.0"}`), &version); err != nil || version.fabo() != "0.5.0" {
		t.Fatalf("older /version: %q, %v", version.fabo(), err)
	}
}

func TestEngineCardDisclosure(t *testing.T) {
	_, s := newFakeEngine(t)
	tt := ui.NewTester(s.view, 1180, 520)
	if tt.HasText("Version") || tt.HasText("Stop VM") {
		t.Fatalf("engine details are visible by default: %q", tt.Texts())
	}
	click(t, tt, "Linux VM")
	waitDesktop(t, tt, func() bool { return tt.HasText("v0.4.0") })
	stop, found := tt.Find("Stop engine")
	if !tt.HasText("Version") || !found || stop.Y+stop.H > 520 {
		t.Fatalf("expanded engine details do not fit: %v, %q", stop, tt.Texts())
	}
	click(t, tt, "Linux VM")
	if tt.HasText("Version") || tt.HasText("Stop VM") {
		t.Fatalf("engine details are visible after collapsing: %q", tt.Texts())
	}
}
