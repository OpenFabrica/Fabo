//go:build linux || (darwin && cgo)

package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/OpenFabrica/Fabo/engine/internal/localengine"
)

// TestResourcesNetworkMode checks how /fabo/resources reads, keeps and
// validates the VM's network mode.
func TestResourcesNetworkMode(t *testing.T) {
	dir := t.TempDir()
	// OpenState wants a private directory.
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	state, err := localengine.OpenState(dir)
	if err != nil {
		t.Fatal(err)
	}
	s := &supervisor{state: state, vm: &vmConfiguration{
		minCPU: 1, maxCPU: 8, minMemory: 1024, maxMemory: 8192,
		settings: localengine.VMSettings{CPUs: 2, MemoryMiB: 2048},
	}}
	call := func(method, body string) (int, map[string]any) {
		t.Helper()
		w := httptest.NewRecorder()
		s.resources(w, httptest.NewRequest(method, "/fabo/resources", strings.NewReader(body)))
		var out map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out
	}
	saved := func() localengine.VMSettings {
		t.Helper()
		settings, err := state.LoadVMSettings(localengine.VMSettings{})
		if err != nil {
			t.Fatal(err)
		}
		return settings
	}

	if code, out := call(http.MethodGet, ""); code != http.StatusOK || out["network"] != "user" || out["running_network"] != "" {
		t.Fatalf("default: %d %v", code, out)
	}
	if code, out := call(http.MethodPost, `{"cpus":2,"memory_mib":2048,"network":"nat"}`); code != http.StatusOK || out["network"] != "nat" {
		t.Fatalf("switch to NAT: %d %v", code, out)
	}
	// A client that predates the setting posts only the size.
	if code, out := call(http.MethodPost, `{"cpus":4,"memory_mib":4096}`); code != http.StatusOK || out["network"] != "nat" {
		t.Fatalf("size-only post: %d %v", code, out)
	}
	if got := saved(); got.Network != "nat" || got.CPUs != 4 {
		t.Fatalf("saved %+v", got)
	}
	if code, _ := call(http.MethodPost, `{"cpus":4,"memory_mib":4096,"network":"bridged"}`); code != http.StatusBadRequest {
		t.Fatalf("unknown mode accepted: %d", code)
	}
	if got := saved(); got.Network != "nat" {
		t.Fatalf("rejected mode was saved: %+v", got)
	}
}
