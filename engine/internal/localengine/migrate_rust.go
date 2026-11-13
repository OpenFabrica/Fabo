//go:build linux || darwin

package localengine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"
)

// MigrationReport counts the objects installed from an offline Rust snapshot.
type MigrationReport struct {
	Images, Volumes, Networks, Containers int
	Settings                              []string `json:",omitempty"`
}

// RustMigration identifies the offline source and the new, empty destination.
// SourceConfig opts into copying the saved daemon settings. ValidateVM uses
// the same resource limits as VM startup when resource settings are present.
type RustMigration struct {
	Snapshot, Destination, SourceSocket string
	Shares                              []string
	SourceConfig, SourceData            string
	ValidateVM                          func(VMSettings) error
}

// MigrateRust installs a read-only Rust store snapshot into a new Go state
// directory. On macOS the snapshot and target must be on the same APFS
// volume. On Linux SourceData identifies the offline agent data tree.
func MigrateRust(ctx context.Context, options RustMigration) (MigrationReport, error) {
	destination := options.Destination
	var report MigrationReport
	if err := checkRustDaemonStopped(options.SourceSocket); err != nil {
		return report, err
	}
	sourcePath, err := filepath.EvalSymlinks(options.Snapshot)
	if err != nil {
		return report, err
	}
	sourcePath, err = filepath.Abs(sourcePath)
	if err != nil {
		return report, err
	}
	destination, err = filepath.Abs(destination)
	if err != nil {
		return report, err
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(destination))
	if err != nil {
		return report, fmt.Errorf("open the destination parent directory: %w", err)
	}
	destination = filepath.Join(parent, filepath.Base(destination))
	if err := checkMigrationDestination(sourcePath, destination); err != nil {
		return report, err
	}
	shares, err := CanonicalShareRoots(options.Shares)
	if err != nil {
		return report, fmt.Errorf("open the Go VM share directories: %w", err)
	}
	source, err := os.OpenRoot(sourcePath)
	if err != nil {
		return report, err
	}
	defer source.Close()
	data, err := openRustMigrationData(source, options.SourceData, destination)
	if err != nil {
		return report, err
	}
	if data != nil {
		defer data.Close()
	}
	stage, err := os.MkdirTemp(parent, ".migrate-kiri-")
	if err != nil {
		return report, err
	}
	defer os.RemoveAll(stage)

	state := &State{dir: stage}
	settings, err := migrateRustSettings(options.SourceConfig, state, options.ValidateVM)
	if err != nil {
		return report, err
	}
	index, err := migrateRustImages(ctx, source, stage, destination)
	if err != nil {
		return report, err
	}
	if err := saveJSON(stage, "image-index.json", index); err != nil {
		return report, err
	}
	if err := migrateRustScans(ctx, source, stage, index); err != nil {
		return report, fmt.Errorf("import image scan history: %w", err)
	}
	volumes, err := migrateRustVolumes(ctx, source, state)
	if err != nil {
		return report, err
	}
	networks, err := migrateRustNetworks(source, state)
	if err != nil {
		return report, err
	}
	vm := runtime.GOOS == "darwin"
	e := &Engine{state: state, imageIndex: index, images: index.references(nil), networks: networks,
		containers: make(map[string]*container), remoteGuest: vm, guestDisk: vm, guestShares: shares, engineSocket: options.SourceSocket}
	if err := e.migrateRustContainers(ctx, source, destination); err != nil {
		return report, err
	}
	if err := migrateRustData(ctx, source, data, stage, e); err != nil {
		return report, err
	}
	imported := MigrationReport{
		Images: len(index.Overrides) + len(index.Dangling), Volumes: volumes,
		Networks: len(networks), Containers: len(e.containers), Settings: settings,
	}
	// The launcher selects Go after a completed import. Save the report
	// inside the stage so it becomes visible with the imported data.
	if err := saveJSON(stage, "rust-import.json", imported); err != nil {
		return report, err
	}
	if err := ctx.Err(); err != nil {
		return report, err
	}
	if err := checkRustDaemonStopped(options.SourceSocket); err != nil {
		return report, err
	}
	// A daemon may have created the destination since preflight. Publishing
	// must fail even if that directory happens to be empty.
	if err := publishRustMigration(stage, destination); err != nil {
		return report, fmt.Errorf("publish the new Go state directory: %w", err)
	}
	return imported, nil
}

func checkRustDaemonStopped(socket string) error {
	info, err := os.Lstat(socket)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("source socket path is not a Unix socket")
	}
	conn, err := net.DialTimeout("unix", socket, time.Second)
	if err == nil {
		_ = conn.Close()
		return fmt.Errorf("source Rust daemon is still running; shut it down before migration")
	}
	if errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return fmt.Errorf("check the source Rust daemon socket: %w", err)
}

func checkMigrationDestination(source, dest string) error {
	relative, err := filepath.Rel(source, dest)
	if err != nil {
		return err
	}
	if relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return fmt.Errorf("destination must be outside the source snapshot")
	}
	if _, err := os.Lstat(dest); err == nil {
		return fmt.Errorf("destination %q already exists", dest)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

func readRustIndex[T any](source *os.Root, file string) ([]T, error) {
	raw, err := source.ReadFile(file)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var records []T
	if err := json.Unmarshal(raw, &records); err != nil {
		return nil, fmt.Errorf("read Rust %s: %w", file, err)
	}
	return records, nil
}
