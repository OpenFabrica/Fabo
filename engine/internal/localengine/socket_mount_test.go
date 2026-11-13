//go:build linux || darwin

package localengine

import (
	"testing"

	"github.com/libracoder/kiri/engine/internal/guestwire"
)

func TestVMEngineSocketUsesDedicatedGuestRelay(t *testing.T) {
	e := &Engine{remoteGuest: true, engineSocket: "/private/kiri-go/run/docker.sock"}
	for _, source := range []string{"/var/run/docker.sock", "/run/docker.sock", "/private/var/run/docker.sock", e.engineSocket} {
		value, err := (guestwire.BindMount{Source: source, Target: "/var/run/docker.sock", ReadOnly: true}).Encode()
		if err != nil {
			t.Fatal(err)
		}
		mapped, err := e.runtimeSocketMounts([]string{value})
		if err != nil {
			t.Fatal(err)
		}
		m, err := guestwire.DecodeBindMount(mapped[0])
		if err != nil || m.Source != guestwire.EngineSocketPath || !m.ReadOnly {
			t.Fatalf("unexpected mapping for %s: %+v, %v", source, m, err)
		}
		if _, err := mapGuestMounts(nil, mapped); err != nil {
			t.Fatalf("socket mapping required a filesystem share: %v", err)
		}
	}
}
