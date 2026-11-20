package main

import (
	"encoding/csv"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"
)

// openScanTab opens shop/api:dev's panel on its Security tab.
func openScanTab(t *testing.T, s *desktop, tt *ui.Tester) {
	t.Helper()
	s.navigate(imagesPage)
	tt.Frame()
	click(t, tt, "Details for shop/api:dev")
	// "Security" also titles a table column, so the tab is chosen directly.
	s.tab[imagesPage] = 1
	tt.Frame()
}

// TestFindingOpensOntoDetails checks that a click on a finding shows what
// the scan knows of it: the description, the installed package, the fix,
// the layer and the advisory, which opens in the browser.
func TestFindingOpensOntoDetails(t *testing.T) {
	_, s := newFakeEngine(t)
	tt := ui.NewTester(s.view, 1180, 760)
	openScanTab(t, s, tt)
	if tt.HasText("zlib inflate reads past the window") {
		t.Fatal("details shown before the finding was opened")
	}
	click(t, tt, "Details for CVE-2026-1001")
	if m := has(tt, "=zlib inflate reads past the window on a crafted stream.", "zlib 1.3.0", "#2 RUN apk add --no-cache nodejs"); m != "" {
		t.Fatalf("missing %q in %q", m, tt.Texts())
	}
	click(t, tt, "Open https://nvd.nist.gov/vuln/detail/CVE-2026-1001")
	if got := tt.OpenedURLs(); !slices.Equal(got, []string{"https://nvd.nist.gov/vuln/detail/CVE-2026-1001"}) {
		t.Fatalf("opened %v", got)
	}

	// A finding with no fix nor advisory says so and links nowhere; one
	// from the base image says where it came from.
	click(t, tt, "Details for CVE-2026-2002")
	if m := has(tt, "No fix yet", "#1 ADD rootfs.tar.xz /", "base image"); m != "" {
		t.Fatalf("missing %q in %q", m, tt.Texts())
	}
	click(t, tt, "Details for CVE-2026-1001")
	if tt.HasText("zlib inflate reads past the window") {
		t.Fatal("a second click left the finding open")
	}
}

// TestExportScanReport exports the last scan in each format through the
// save dialog: the table and the report from the scan, the SBOM from the
// engine's copy of the same scan.
func TestExportScanReport(t *testing.T) {
	_, s := newFakeEngine(t)
	dir := t.TempDir()
	var asked []string
	s.chooser = func(save bool, name string) string {
		asked = append(asked, name)
		// The user drops the extension; the export puts it back.
		return filepath.Join(dir, strings.TrimSuffix(strings.TrimSuffix(name, ".json"), ".csv"))
	}
	tt := ui.NewTester(s.view, 1180, 760)
	openScanTab(t, s, tt)
	// The names carry the scan's local day.
	day := time.Unix(1795084368, 0).Format("20060102")
	csvName, jsonName, sbomName := "shop-api-dev-scan-"+day+".csv", "shop-api-dev-scan-"+day+".json", "shop-api-dev-sbom-"+day+".cdx.json"
	export := func(format string) {
		t.Helper()
		click(t, tt, "Export report")
		if err := tt.ChooseMenuItem(format); err != nil {
			t.Fatal(err)
		}
		tt.Frame()
	}
	export("CSV table")
	export("JSON report")
	export("SBOM (CycloneDX)")
	if want := []string{csvName, jsonName, sbomName}; !slices.Equal(asked, want) {
		t.Fatalf("suggested %q", asked)
	}

	f, err := os.Open(filepath.Join(dir, csvName))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	rows, err := csv.NewReader(f).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 || rows[0][0] != "Vulnerability" || rows[1][0] != "CVE-2026-2002" || rows[2][0] != "CVE-2026-1001" {
		t.Fatalf("rows: %q", rows)
	}
	// Layers count from 1, and text from the database cannot become a
	// spreadsheet formula.
	if got := rows[2]; got[6] != "2" || got[8] != "false" || got[10] != "'=zlib inflate reads past the window on a crafted stream." {
		t.Fatalf("row: %q", got)
	}

	var report scanReport
	data, err := os.ReadFile(filepath.Join(dir, jsonName))
	if err == nil {
		err = json.Unmarshal(data, &report)
	}
	if err != nil {
		t.Fatal(err)
	}
	if report.Image != "shop/api:dev" || report.Scanned != "2026-11-19T10:32:48Z" || report.Grype != "0.120.1" ||
		report.Counts["High"] != 1 || report.Counts["Medium"] != 1 || len(report.Vulns) != 2 || report.Vulns[1].Description == "" {
		t.Fatalf("report: %+v", report)
	}

	sbom, err := os.ReadFile(filepath.Join(dir, sbomName))
	if err != nil || !strings.Contains(string(sbom), "CycloneDX") {
		t.Fatalf("sbom %q: %v", sbom, err)
	}
	if !tt.HasText("Exported to " + sbomName) {
		t.Fatalf("no notice in %q", tt.Texts())
	}
}
