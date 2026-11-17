//go:build linux || darwin

package localengine

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"runtime"
	"testing"
)

// The engine names the host-guest protocol it speaks: as a fabo-init
// component, which docker version lists, and in /version and /info.
func TestVersionReportsGuestProtocol(t *testing.T) {
	e := &Engine{ctx: t.Context(), containers: map[string]*container{}}
	get := func(path string, out any) {
		t.Helper()
		w := httptest.NewRecorder()
		e.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code != http.StatusOK {
			t.Fatalf("%s: %d", path, w.Code)
		}
		if err := json.NewDecoder(w.Body).Decode(out); err != nil {
			t.Fatal(err)
		}
	}
	var version struct {
		FaboGuestProtocol string
		Components        []struct {
			Name    string
			Details map[string]string
		}
	}
	get("/version", &version)
	if version.FaboGuestProtocol != "0.6" {
		t.Fatalf("/version protocol %q", version.FaboGuestProtocol)
	}
	found := false
	for _, c := range version.Components {
		if c.Name == "fabo-init" && c.Details["Protocol"] == "0.6" {
			found = true
		}
		// docker version shows the Engine's Go version from its details.
		if c.Name == "Engine" && c.Details["GoVersion"] != runtime.Version() {
			t.Fatalf("Engine GoVersion %q", c.Details["GoVersion"])
		}
	}
	if !found {
		t.Fatalf("no fabo-init component with protocol 0.5: %+v", version.Components)
	}
	var info struct{ FaboGuestProtocol string }
	get("/info", &info)
	if info.FaboGuestProtocol != "0.6" {
		t.Fatalf("/info protocol %q", info.FaboGuestProtocol)
	}
}
