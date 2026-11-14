//go:build linux || darwin

package localengine

import (
	"testing"

	"github.com/OpenFabrica/FabriBay/engine/internal/guestwire"
)

// A record written before the rename takes today's bridge names and VM
// paths; names that were never Kiri's stay as they are.
func TestLegacyRecordTakesTodaysNames(t *testing.T) {
	for old, want := range map[string]string{
		"kiri0":           guestwire.DefaultBridge,
		"kiri14b95decaa":  "fabo14b95decaa",
		"fabo14b95decaa":  "fabo14b95decaa",
		"kiri-notabridge": "kiri-notabridge",
	} {
		if got := renameLegacyBridge(old); got != want {
			t.Errorf("bridge %q = %q, want %q", old, got, want)
		}
	}
	for old, want := range map[string]string{
		"/var/lib/kiri/volumes/v1/_data\t/data": guestwire.VolumeRoot + "/v1/_data\t/data",
		"/mnt/kiri-host/Users/me/app\t/app":     guestwire.HostSharePath + "/Users/me/app\t/app",
		"/var/lib/kirifoo/x\t/y":                "/var/lib/kirifoo/x\t/y",
	} {
		if got := renameLegacyMount(old); got != want {
			t.Errorf("mount %q = %q, want %q", old, got, want)
		}
	}
	rec := savedContainer{}
	rec.Run.Network.Bridge = "kiri0"
	rec.Run.Mounts = []string{"/var/lib/kiri/volumes/v1/_data\t/data"}
	rec.ExtraNetworks = []containerEndpoint{{Network: guestwire.Network{Bridge: "kiri0123456789"}}}
	renameLegacyRecord(&rec)
	if rec.Run.Network.Bridge != guestwire.DefaultBridge || rec.Run.Mounts[0] != guestwire.VolumeRoot+"/v1/_data\t/data" || rec.ExtraNetworks[0].Network.Bridge != "fabo0123456789" {
		t.Fatalf("record: %+v", rec)
	}
}
