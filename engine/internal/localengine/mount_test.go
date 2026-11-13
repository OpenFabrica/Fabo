//go:build linux || darwin

package localengine

import (
	"encoding/json"
	"slices"

	"github.com/libracoder/kiri/engine/internal/guestwire"
	"testing"
)

func TestParseDockerBindMounts(t *testing.T) {
	host := map[string]json.RawMessage{
		"Binds":          []byte(`["/host/out:/out"]`),
		"Mounts":         []byte(`[{"Type":"bind","Source":"/host/data","Target":"/data","ReadOnly":true}]`),
		"ReadonlyRootfs": []byte("true"),
	}
	got, volumes, _, ro, err := parseMounts(host, false)
	want := []bindMount{
		{spec: guestwire.BindMount{Source: "/host/out", Target: "/out"}, create: true},
		{spec: guestwire.BindMount{Source: "/host/data", Target: "/data", ReadOnly: true}},
	}
	if err != nil || len(volumes) != 0 || !ro || !slices.Equal(got, want) || len(host) != 0 {
		t.Fatalf("mounts=%+v root read-only=%v err=%v", got, ro, err)
	}
	for _, spec := range []string{`["/host:/proc/x"]`, `["/host:/a/../b"]`, `["../myvolume:/data"]`} {
		if _, _, _, _, err := parseMounts(map[string]json.RawMessage{"Binds": []byte(spec)}, false); err == nil {
			t.Errorf("accepted %s", spec)
		}
	}
}

func TestVolumeCopyUpOptions(t *testing.T) {
	host := map[string]json.RawMessage{
		"Binds":  []byte(`["seeded:/one","legacy:/two:ro,nocopy"]`),
		"Mounts": []byte(`[{"Type":"volume","Source":"modern","Target":"/three","VolumeOptions":{"NoCopy":true,"DriverConfig":{"Name":"local","Options":{"type":"tmpfs","device":"tmpfs","o":"size=32m"}},"Labels":{"team":"dev"}}}]`),
	}
	binds, volumes, _, _, err := parseMounts(host, false)
	if err != nil || len(binds) != 0 || len(volumes) != 3 {
		t.Fatalf("binds=%+v volumes=%+v err=%v", binds, volumes, err)
	}
	if volumes[0].noCopy || !volumes[1].noCopy || !volumes[1].readOnly || !volumes[2].noCopy || volumes[2].labels["team"] != "dev" {
		t.Fatalf("unexpected volume options: %+v", volumes)
	}
	if driver, err := localVolumeMount(volumes[2].options); err != nil || driver == nil || driver.Type != "tmpfs" || driver.Device != "tmpfs" || driver.Options != "size=32m" {
		t.Fatalf("structured local driver options: %+v, %v", driver, err)
	}
}

func TestTmpfsMountsNormalizeAndRejectConflictingTargets(t *testing.T) {
	host := map[string]json.RawMessage{
		"Tmpfs":  []byte(`{"/cache/":"size=4m,mode=1770"}`),
		"Mounts": []byte(`[{"Type":"tmpfs","Target":"/sealed","ReadOnly":true,"TmpfsOptions":{"SizeBytes":2097152,"Mode":365,"Options":[["nr_inodes","32"]]}}]`),
	}
	_, _, mounts, _, err := parseMounts(host, false)
	if err != nil || len(mounts) != 2 || len(host) != 0 {
		t.Fatalf("tmpfs mounts %+v, error %v", mounts, err)
	}
	if !mounts[0].ReadOnly() || !slices.Equal(mounts[0].Options, []string{"size=2097152", "mode=555", "nr_inodes=32", "ro"}) ||
		mounts[1].Target != "/cache" || mounts[1].ReadOnly() {
		t.Fatalf("unexpected normalized tmpfs mounts: %+v", mounts)
	}
	for target, options := range map[string]string{"/proc/cache": "", "/a/../cache": "", "/shared/": "", "/other": "mode=0700\n"} {
		value, err := json.Marshal(map[string]string{target: options})
		if err != nil {
			t.Fatal(err)
		}
		_, _, _, _, err = parseMounts(map[string]json.RawMessage{"Tmpfs": value, "Binds": []byte(`["volume:/shared"]`)}, false)
		if err == nil {
			t.Errorf("accepted tmpfs %q with options %q", target, options)
		}
	}
}

func TestWSLEnginePipeMountNormalizesToLinuxSocket(t *testing.T) {
	binds, _, _, _, err := parseMounts(map[string]json.RawMessage{
		"Binds": []byte(`["\\\\.\\pipe\\docker_engine:/var/run/docker.sock:ro"]`),
	}, true)
	if err != nil || len(binds) != 1 || binds[0].spec.Source != "/var/run/docker.sock" || !binds[0].spec.ReadOnly {
		t.Fatalf("Windows pipe mount: %+v, %v", binds, err)
	}
}
