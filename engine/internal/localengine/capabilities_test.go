//go:build linux || darwin

package localengine

import (
	"encoding/json"
	"github.com/OpenFabrica/FabriBay/engine/internal/guestwire"
	"testing"
)

func TestParseCapabilities(t *testing.T) {
	h := map[string]json.RawMessage{"CapAdd": json.RawMessage(`["NET_ADMIN"]`), "CapDrop": json.RawMessage(`["ALL"]`)}
	c, err := parseCapabilities(h)
	if err != nil {
		t.Fatal(err)
	}
	mask, err := c.mask()
	if err != nil || mask != 1<<12 || len(h) != 0 {
		t.Fatalf("%#v, %x, %v", h, mask, err)
	}
	for _, raw := range []string{`["INVALID"]`, `[1]`} {
		if _, err := parseCapabilities(map[string]json.RawMessage{"CapAdd": json.RawMessage(raw)}); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
	if guestwire.EffectiveCapabilities(guestwire.CapabilityNone) != 0 {
		t.Fatal("empty capability mask became defaults")
	}
}
