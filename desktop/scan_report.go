package main

// Exporting an image's last scan: its findings as a CSV table or a JSON
// report written here, or the CycloneDX SBOM the engine stored with it.

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/egoist/mygo/ui"
)

// scanFormat is a format a scan exports to.
type scanFormat int

const (
	scanCSV scanFormat = iota
	scanJSON
	scanSBOM
)

// extension is the file extension a format's files take.
func (f scanFormat) extension() string {
	switch f {
	case scanJSON:
		return ".json"
	case scanSBOM:
		return ".cdx.json"
	}
	return ".csv"
}

// fileKind is what the save dialog shows for a format.
func (f scanFormat) fileKind() fileKind {
	filter := tr("CSV 表格", "CSV table")
	switch f {
	case scanJSON:
		filter = tr("JSON 报告", "JSON report")
	case scanSBOM:
		filter = "CycloneDX SBOM"
	}
	// The SBOM's ".cdx.json" is a JSON file to the dialog.
	extension := strings.TrimPrefix(filepath.Ext(f.extension()), ".")
	return fileKind{tr("导出报告", "Export report"), tr("导出", "Export"), filter, []string{extension}}
}

// sortedFindings returns a scan's findings, the worst first, keeping the
// scan's order within a severity.
func sortedFindings(result *scanResult) []finding {
	found := append([]finding(nil), result.Vulns...)
	sort.SliceStable(found, func(i, j int) bool { return severityRank(found[i].Severity) < severityRank(found[j].Severity) })
	return found
}

// scanFileName suggests a file for an image's scan: the image's name, the
// day of the scan and the format's extension.
func scanFileName(img image, result *scanResult, format scanFormat) string {
	base := strings.TrimSuffix(imageFileName(img), ".tar")
	if format == scanSBOM {
		base += "-sbom"
	} else {
		base += "-scan"
	}
	return base + "-" + time.Unix(result.Scanned, 0).Format("20060102") + format.extension()
}

// exportMenu is the button that offers the formats an image's last scan
// exports to.
func (s *desktop) exportMenu(c *ui.Context, p palette, img image, result *scanResult) {
	text := tr("导出报告", "Export report")
	buttonElement(c.Key("export-scan"), p, text, text, "", ghostButton, s.working["export-scan:"+img.ID]).PopoverMenuButton(func(m *ui.Menu) {
		if m.Item(tr("CSV 表格", "CSV table")).Chosen() {
			s.exportScan(img, result, scanCSV)
		}
		if m.Item(tr("JSON 报告", "JSON report")).Chosen() {
			s.exportScan(img, result, scanJSON)
		}
		if m.Item("SBOM (CycloneDX)").Chosen() {
			s.exportScan(img, result, scanSBOM)
		}
	})
}

// exportScan asks where to save an image's scan in format and writes it
// there: the table and the report from the scan the window holds, the SBOM
// from the engine's copy of the same scan.
func (s *desktop) exportScan(img image, result *scanResult, format scanFormat) {
	key := "export-scan:" + img.ID
	if s.working[key] || result == nil {
		return
	}
	s.withFile(true, scanFileName(img, result, format), format.fileKind(), func(path string) {
		// The dialog may hand back the name without its extension, or with
		// only the ".cdx" of an SBOM's ".cdx.json".
		if !strings.EqualFold(filepath.Ext(path), filepath.Ext(format.extension())) {
			path = strings.TrimSuffix(path, ".cdx") + format.extension()
		}
		s.working[key] = true
		ref := img.ref()
		s.run(2*time.Minute, func(ctx context.Context) func() {
			var err error
			switch format {
			case scanSBOM:
				err = s.engine.saveSBOM(ctx, ref, result.Scanned, path)
			case scanJSON:
				err = writeWhole(path, func(w io.Writer) error { return writeScanJSON(w, ref, result) })
			default:
				err = writeWhole(path, func(w io.Writer) error { return writeScanCSV(w, result) })
			}
			return func() {
				delete(s.working, key)
				s.settle(err, fmt.Sprintf(tr("已导出到 %s", "Exported to %s"), filepath.Base(path)))
			}
		})
	})
}

// saveSBOM streams the CycloneDX SBOM the engine stored with an image's
// scan at the given time into path.
func (e engineClient) saveSBOM(ctx context.Context, ref string, at int64, path string) error {
	q := url.Values{"name": {ref}, "at": {strconv.FormatInt(at, 10)}}
	return e.download(ctx, "/fabo/images/sbom?"+q.Encode(), path)
}

// scanReport is the JSON report of a scan: what was scanned, when, with
// which scanner and database, then the findings, the worst first.
type scanReport struct {
	Image    string         `json:"image"`
	ID       string         `json:"id,omitempty"`
	Scanned  string         `json:"scanned"`
	Packages int            `json:"packages"`
	DB       string         `json:"db,omitempty"`
	Grype    string         `json:"grype,omitempty"`
	Counts   map[string]int `json:"counts"`
	Vulns    []finding      `json:"vulns"`
}

// writeScanJSON writes the JSON report of the scan of the image ref.
func writeScanJSON(w io.Writer, ref string, result *scanResult) error {
	report := scanReport{
		Image: ref, ID: result.ID, Scanned: time.Unix(result.Scanned, 0).UTC().Format(time.RFC3339),
		Packages: result.Packages, DB: result.DB, Grype: result.Grype,
		Counts: map[string]int{}, Vulns: sortedFindings(result),
	}
	for _, f := range report.Vulns {
		report.Counts[f.Severity]++
	}
	out := json.NewEncoder(w)
	out.SetIndent("", "  ")
	return out.Encode(report)
}

// writeScanCSV writes a scan's findings as a table, one row a finding, the
// worst first. Layers count from 1, as the Layers tab shows them.
func writeScanCSV(w io.Writer, result *scanResult) error {
	out := csv.NewWriter(w)
	// The header stays in English whatever the window's language, so
	// that tools reading the table find the same columns.
	header := []string{"Vulnerability", "Severity", "Package", "Version", "Type", "Fixed in",
		"Layer", "Created by", "Base image", "URL", "Description"}
	if err := out.Write(header); err != nil {
		return err
	}
	for _, f := range sortedFindings(result) {
		layer, by, base := "", "", ""
		if f.Layer != nil {
			layer = strconv.Itoa(*f.Layer + 1)
		}
		if f.By != nil {
			by = strings.Join(strings.Fields(*f.By), " ")
		}
		if f.Base != nil {
			base = strconv.FormatBool(*f.Base)
		}
		row := []string{f.ID, f.Severity, f.Package, f.Version, f.Kind, strings.Join(f.Fixed, " "),
			layer, by, base, f.URL, f.Description}
		for i, cell := range row {
			row[i] = inertCell(cell)
		}
		if err := out.Write(row); err != nil {
			return err
		}
	}
	out.Flush()
	return out.Error()
}

// inertCell keeps a spreadsheet from reading a cell as a formula: text
// from the vulnerability database that starts with =, +, - or @ gets a
// leading apostrophe, which spreadsheets show as plain text.
func inertCell(cell string) string {
	if cell != "" && strings.ContainsRune("=+-@\t\r", rune(cell[0])) {
		return "'" + cell
	}
	return cell
}
