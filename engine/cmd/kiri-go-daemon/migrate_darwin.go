//go:build cgo

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
	snapshot := flags.String("snapshot", "", "offline Rust image store snapshot")
	state := flags.String("state", "", "new Go state directory (must not exist)")
	config := flags.String("source-config", "", "optional offline Rust daemon settings directory")
	home := os.Getenv("HOME")
	sourceSocket := flags.String("source-socket", filepath.Join(home, ".kiri", "run", "docker.sock"), "original Rust daemon socket to check")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if *snapshot == "" || *state == "" || !filepath.IsAbs(*sourceSocket) || flags.NArg() != 0 {
		return fmt.Errorf("migrate-rust requires -snapshot, -state and an absolute source socket path")
	}
	var guest guestOptions
	report, err := localengine.MigrateRust(ctx, localengine.RustMigration{
		Snapshot: *snapshot, Destination: *state, SourceSocket: *sourceSocket, Shares: standardMacShares(),
		SourceConfig: *config, ValidateVM: guest.vmConfiguration().validate,
	})
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(report)
}
