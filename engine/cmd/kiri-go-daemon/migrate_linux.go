package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/libracoder/kiri/engine/internal/localengine"
)

func migrateRustCommand(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("migrate-rust", flag.ContinueOnError)
	snapshot := flags.String("snapshot", "/var/lib/kiri-daemon/.local/share/kiri", "offline Rust image and metadata store")
	data := flags.String("source-data", "/var/lib/kiri", "offline Rust agent directory containing writable layers and volumes")
	state := flags.String("state", "/var/lib/kiri-daemon/.kiri/go-engine", "new Go state directory (must not exist)")
	socket := flags.String("source-socket", "/run/kiri/docker.sock", "original daemon socket to check")
	config := flags.String("source-config", "", "optional offline Rust daemon settings directory")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if *snapshot == "" || *data == "" || *state == "" || !filepath.IsAbs(*socket) || flags.NArg() != 0 {
		return fmt.Errorf("migrate-rust requires source directories, a destination and an absolute source socket path")
	}
	if os.Geteuid() != 0 {
		return fmt.Errorf("migrate-rust must run as root to preserve file ownership and overlay metadata")
	}
	report, err := localengine.MigrateRust(ctx, localengine.RustMigration{
		Snapshot: *snapshot, Destination: *state, SourceSocket: *socket, SourceConfig: *config, SourceData: *data,
	})
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(report)
}
