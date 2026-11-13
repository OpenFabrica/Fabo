//go:build linux || darwin

package localengine

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestBuilderSettingsPatchPersistsAndOverridesApply(t *testing.T) {
	t.Setenv("KIRI_BUILD_CACHE_GB", "8")
	t.Setenv("KIRI_BUILDER_IDLE_SECS", "0")
	state := &State{dir: t.TempDir()}
	e := &Engine{state: state}
	post := func(body string, status int) *httptest.ResponseRecorder {
		t.Helper()
		w := httptest.NewRecorder()
		e.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/kiri/builder", strings.NewReader(body)))
		if w.Code != status {
			t.Fatalf("status %d, expected %d: %s", w.Code, status, w.Body.String())
		}
		return w
	}
	post(`{"cache_gb":3,"idle_secs":42}`, http.StatusOK)
	post(`{"cache_gb":6,"idle_secs":604801}`, http.StatusBadRequest)
	w := post(`{"cache_gb":null}`, http.StatusOK)
	var report struct {
		CacheGB    *uint64 `json:"cache_gb"`
		CacheLimit uint64  `json:"cache_limit"`
		IdleSecs   uint64  `json:"idle_secs"`
		Overridden bool
	}
	if err := json.Unmarshal(w.Body.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.CacheGB != nil || report.CacheLimit != 8*builderGiB || report.IdleSecs != 0 || !report.Overridden {
		t.Fatalf("unexpected effective settings: %s", w.Body.String())
	}
	saved, err := state.loadBuilderSettings()
	if err != nil || saved.CacheGB != nil || saved.IdleSecs == nil || *saved.IdleSecs != 42 {
		t.Fatalf("partial or rejected update changed the saved timeout: %+v, %v", saved, err)
	}
}
