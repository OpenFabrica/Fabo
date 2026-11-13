package localengine

import (
	"encoding/binary"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMigrationRejectsUnsupportedVolumeWithoutPublishing(t *testing.T) {
	base := t.TempDir()
	source, destination := filepath.Join(base, "old"), filepath.Join(base, "new")
	writeMigrationTestDisk(t, source)
	volumes := filepath.Join(source, "volumes.json")
	before := `[{"name":"remote","options":{"type":"nfs"}}]`
	if err := os.WriteFile(volumes, []byte(before), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := MigrateRust(t.Context(), RustMigration{Snapshot: source, Destination: destination, SourceSocket: filepath.Join(base, "old.sock"), Shares: []string{base}})
	if err == nil || !strings.Contains(err.Error(), "local driver options") {
		t.Fatalf("unexpected migration result: %v", err)
	}
	if _, err := os.Stat(destination); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("partial destination: %v", err)
	}
	entries, err := os.ReadDir(base)
	if err != nil || len(entries) != 1 || entries[0].Name() != "old" {
		t.Fatalf("migration left stage files: %v %v", entries, err)
	}
	raw, err := os.ReadFile(volumes)
	if err != nil || string(raw) != before {
		t.Fatalf("snapshot changed: %q %v", raw, err)
	}
}

func TestMigrationRequiresDiskWhenTheSourceHasVolumes(t *testing.T) {
	base := t.TempDir()
	source, destination := filepath.Join(base, "old"), filepath.Join(base, "new")
	if err := os.Mkdir(source, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "volumes.json"), []byte(`[{"name":"cache"}]`), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := MigrateRust(t.Context(), RustMigration{Snapshot: source, Destination: destination, SourceSocket: filepath.Join(base, "old.sock")})
	if err == nil || !strings.Contains(err.Error(), "disk is required") {
		t.Fatalf("unexpected migration result: %v", err)
	}
	if _, err := os.Stat(destination); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("partial destination: %v", err)
	}
}

// Rejecting one setting must roll back an earlier setting saved in the stage.
func TestMigrationRejectsSettingsWithoutPublishing(t *testing.T) {
	base := t.TempDir()
	source, destination := filepath.Join(base, "old"), filepath.Join(base, "new")
	writeMigrationTestDisk(t, source)
	config := filepath.Join(base, "config")
	if err := os.Mkdir(config, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(config, "vm.json"), []byte(`{"cpus":3,"memory_mib":2048}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(config, "domains.json"), []byte(`{"http":9000,"https":9000}`), 0600); err != nil {
		t.Fatal(err)
	}
	validated := false
	_, err := MigrateRust(t.Context(), RustMigration{
		Snapshot: source, Destination: destination, SourceSocket: filepath.Join(base, "old.sock"),
		SourceConfig: config, ValidateVM: func(vm VMSettings) error {
			validated = vm.CPUs == 3 && vm.MemoryMiB == 2048
			return nil
		},
	})
	if !validated || err == nil || !strings.Contains(err.Error(), "distinct ports") {
		t.Fatalf("unexpected migration result: %v", err)
	}
	entries, err := os.ReadDir(base)
	if err != nil || len(entries) != 2 || entries[0].Name() != "config" || entries[1].Name() != "old" {
		t.Fatalf("migration left a destination or staging files: %v %v", entries, err)
	}
}

func writeMigrationTestDisk(t *testing.T, source string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(source, "vm"), 0700); err != nil {
		t.Fatal(err)
	}
	// The old agent may leave a journal to replay even after its daemon exits.
	var disk [2048]byte
	binary.LittleEndian.PutUint16(disk[1024+0x38:], 0xef53)
	binary.LittleEndian.PutUint16(disk[1024+0x3a:], 1)
	binary.LittleEndian.PutUint32(disk[1024+0x60:], 4)
	if err := os.WriteFile(filepath.Join(source, "vm/data.img"), disk[:], 0600); err != nil {
		t.Fatal(err)
	}
}
