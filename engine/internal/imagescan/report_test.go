package imagescan

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadsRustReportWithLayerAttribution(t *testing.T) {
	s := New(t.TempDir())
	id := "sha256:" + strings.Repeat("a", 64)
	dir, err := s.scanDir(id)
	if err != nil {
		t.Fatal(err)
	}
	const at = int64(123)
	path := reportDir(dir, at)
	if err := os.MkdirAll(path, 0700); err != nil {
		t.Fatal(err)
	}
	const critical = `{"vulnerability":{"id":"CVE-2","severity":"Critical","fix":{"state":"fixed","versions":["2"]},"dataSource":"http://plain"},"artifact":{"name":"app","version":"1","locations":[{"layerID":"sha256:diff"}]}}`
	const medium = `{"vulnerability":{"id":"CVE-1","severity":"Medium","dataSource":"https://example.test/advisory"},"artifact":{"name":"zlib","version":"1","locations":[{"layerID":"sha256:blob"}]}}`
	report := `{"descriptor":{"version":"0.120.1","db":{"status":{"built":"2026-11-10T00:00:00Z"}}},"matches":[` + medium + "," + critical + "," + critical + "]}"
	if err := os.WriteFile(filepath.Join(path, reportFile), []byte(report), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, sbomFile), []byte(`{"components":[{},{}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	layers := []Layer{{Digest: "sha256:blob", By: "ADD rootfs /", Base: true}, {DiffID: "sha256:diff", By: "COPY app /"}}
	got, err := s.Open(id, nil, layers)
	if err != nil || got == nil || got.Scanned != at || got.Packages != 2 || len(got.Vulns) != 2 {
		t.Fatalf("read migrated scan: %+v, %v", got, err)
	}
	first, second := got.Vulns[0], got.Vulns[1]
	if first.ID != "CVE-2" || first.Layer == nil || *first.Layer != 1 || *first.Base || first.URL != "" || len(first.Fixed) != 1 {
		t.Fatalf("first finding: %+v", first)
	}
	if second.Layer == nil || *second.Layer != 0 || !*second.Base || second.URL == "" {
		t.Fatalf("second finding: %+v", second)
	}
	history, err := s.History(id, layers)
	if err != nil || len(history) != 1 || history[0].Vulns != 2 || history[0].Fixable != 1 || history[0].Counts["Critical"] != 1 {
		t.Fatalf("history: %+v, %v", history, err)
	}
}
