//go:build linux || darwin

package localengine

import "testing"

func TestParseGuestSignals(t *testing.T) {
	for raw, want := range map[string]uint32{"sigusr1": 10, "SIGTERM": 15, "SIGRTMIN+3": 37, "RTMAX-2": 62, "9": 9} {
		if got, err := parseSignal(raw); err != nil || got != want {
			t.Errorf("parse %q = %d, %v, want %d", raw, got, err, want)
		}
	}
	for _, raw := range []string{"0", "32", "65", "RTMIN+16", "RTMAX-15", "unknown"} {
		if got, err := parseSignal(raw); err == nil {
			t.Errorf("accepted %q as %d", raw, got)
		}
	}
}
