//go:build linux || darwin

package localengine

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/OpenFabrica/Fabo/engine/internal/guestwire"
)

func TestCommitConfigKeepsExplicitlyEmptyEntrypointAfterRestore(t *testing.T) {
	base := Image{
		Entrypoint: []string{"/bin/sh"},
		ConfigBlob: []byte(`{"architecture":"arm64","os":"linux","config":{"Entrypoint":["/bin/sh"],"Volumes":{"/data":{}}}}`),
	}
	// This command starts with the image entrypoint by coincidence. An
	// explicit override of the entrypoint must survive a daemon restart.
	c := &container{
		id:   "1111111111111111111111111111111111111111111111111111111111111111",
		name: "source", state: "created", entrypoint: []string{}, stopSignal: sigTerm,
		run: guestwire.RunSpec{Argv: []string{"/bin/sh", "-c", "echo old"}, Env: []string{"MODE=old"}},
	}
	c.run.ID = c.id
	e := &Engine{state: &State{dir: t.TempDir()}}
	if err := e.state.save(c); err != nil {
		t.Fatal(err)
	}
	saved, err := e.state.restore()
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := e.prepareCommitConfig(t.Context(), base, saved[c.id], nil, []string{"ENV MODE=new"})
	if err != nil {
		t.Fatal(err)
	}
	var cmd, env []string
	if err := json.Unmarshal(cfg.Config["Cmd"], &cmd); err != nil || !slices.Equal(cmd, c.run.Argv) {
		t.Fatalf("committed command: %q, %v", cmd, err)
	}
	if err := json.Unmarshal(cfg.Config["Env"], &env); err != nil || !slices.Equal(env, []string{"MODE=new"}) {
		t.Fatalf("committed environment: %q, %v", env, err)
	}
	if string(cfg.Config["Entrypoint"]) != "[]" || string(cfg.Config["Volumes"]) != `{"/data":{}}` {
		t.Fatalf("committed config: %s, %s", cfg.Config["Entrypoint"], cfg.Config["Volumes"])
	}
}

func TestCommitConfigUsesValidStopSignalForImportedContainer(t *testing.T) {
	base := Image{ConfigBlob: []byte(`{"architecture":"arm64","os":"linux","config":{}}`)}
	e := &Engine{}
	// A zero stop signal in Rust records means the standard SIGTERM.
	for _, tc := range []struct{ stored, want uint32 }{{0, sigTerm}, {37, 37}} {
		c := &container{stopSignal: tc.stored}
		cfg, err := e.prepareCommitConfig(t.Context(), base, c, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		var value string
		if err := json.Unmarshal(cfg.Config["StopSignal"], &value); err != nil {
			t.Fatal(err)
		}
		if signal, err := parseSignal(value); err != nil || signal != tc.want {
			t.Fatalf("committed StopSignal %q, want %d: %v", value, tc.want, err)
		}
	}
}
