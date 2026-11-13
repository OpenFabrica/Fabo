package imagescan

import (
	"cmp"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// Layer identifies one filesystem layer in config order.
type Layer struct {
	Digest, DiffID, By string
	Base               bool
}

type Result struct {
	ID       string    `json:"id"`
	Scanned  int64     `json:"scanned"`
	Packages int       `json:"packages"`
	DB       string    `json:"db"`
	Grype    string    `json:"grype"`
	Vulns    []Finding `json:"vulns"`
}

type Finding struct {
	ID          string   `json:"id"`
	Severity    string   `json:"severity"`
	Package     string   `json:"package"`
	Version     string   `json:"version"`
	Kind        string   `json:"kind"`
	Fixed       []string `json:"fixed"`
	URL         string   `json:"url"`
	Description string   `json:"description"`
	Layer       *int     `json:"layer"`
	By          *string  `json:"by"`
	Base        *bool    `json:"base"`
}

type Summary struct {
	Scanned  int64          `json:"scanned"`
	Counts   map[string]int `json:"counts"`
	Vulns    int            `json:"vulns"`
	Fixable  int            `json:"fixable"`
	Packages int            `json:"packages"`
	DB       string         `json:"db"`
	Grype    string         `json:"grype"`
}

func (r Result) summary() Summary {
	s := Summary{Scanned: r.Scanned, Counts: make(map[string]int), Vulns: len(r.Vulns),
		Packages: r.Packages, DB: r.DB, Grype: r.Grype}
	for _, f := range r.Vulns {
		s.Counts[f.Severity]++
		if len(f.Fixed) > 0 {
			s.Fixable++
		}
	}
	return s
}

func (s *Scanner) scanDir(id string) (string, error) {
	digest, ok := strings.CutPrefix(id, "sha256:")
	if _, err := hex.DecodeString(digest); !ok || len(digest) != 64 || err != nil {
		return "", fmt.Errorf("invalid scan image ID %q", id)
	}
	return filepath.Join(s.root, "scans", digest), nil
}

func reportDir(dir string, at int64) string { return filepath.Join(dir, strconv.FormatInt(at, 10)) }

func scanTimes(dir string) ([]int64, error) {
	files, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var times []int64
	for _, entry := range files {
		if n, err := strconv.ParseInt(entry.Name(), 10, 64); err == nil && n >= 0 &&
			strconv.FormatInt(n, 10) == entry.Name() && entry.IsDir() {
			times = append(times, n)
		}
	}
	slices.SortFunc(times, func(a, b int64) int { return cmp.Compare(b, a) })
	return times, nil
}

// History lists complete reports newest first. Rust reports may lack summaries.
func (s *Scanner) History(id string, layers []Layer) ([]Summary, error) {
	dir, err := s.scanDir(id)
	if err != nil {
		return nil, err
	}
	times, err := scanTimes(dir)
	if err != nil {
		return nil, err
	}
	summaries := make([]Summary, 0, len(times))
	for _, at := range times {
		path := reportDir(dir, at)
		var summary Summary
		if err := readJSON(filepath.Join(path, summaryFile), &summary); err != nil {
			r, err := readReport(path, id, at, layers)
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				return nil, err
			}
			summary = r.summary()
		}
		summaries = append(summaries, summary)
	}
	return summaries, nil
}

// Open reads a particular scan, or the latest if at is nil.
func (s *Scanner) Open(id string, at *int64, layers []Layer) (*Result, error) {
	dir, err := s.scanDir(id)
	if err != nil {
		return nil, err
	}
	if at == nil {
		times, err := scanTimes(dir)
		if err != nil || len(times) == 0 {
			return nil, err
		}
		at = &times[0]
	}
	path := reportDir(dir, *at)
	var result Result
	err = readJSON(filepath.Join(path, resultFile), &result)
	if errors.Is(err, os.ErrNotExist) {
		result, err = readReport(path, id, *at, layers)
	}
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	return &result, err
}

// SBOM opens the published CycloneDX document for an existing scan.
func (s *Scanner) SBOM(id string, at int64) (*os.File, error) {
	dir, err := s.scanDir(id)
	if err != nil {
		return nil, err
	}
	return os.Open(filepath.Join(reportDir(dir, at), sbomFile))
}

func readJSON(path string, dest any) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return json.NewDecoder(f).Decode(dest)
}

func writeJSON(path string, value any) error {
	b, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0600)
}

func readReport(dir, id string, at int64, layers []Layer) (Result, error) {
	var report grypeReport
	if err := readJSON(filepath.Join(dir, reportFile), &report); err != nil {
		return Result{}, err
	}
	var sbom struct{ Components []json.RawMessage }
	if err := readJSON(filepath.Join(dir, sbomFile), &sbom); err != nil && !errors.Is(err, os.ErrNotExist) {
		return Result{}, err
	}
	usedVersion := report.Descriptor.Version
	if usedVersion == "" {
		usedVersion = version
	}
	return Result{ID: id, Scanned: at, Packages: len(sbom.Components), DB: report.Descriptor.DB.Status.Built,
		Grype: usedVersion, Vulns: report.findings(layers)}, nil
}

type grypeReport struct {
	Descriptor struct {
		Version string
		DB      struct{ Status struct{ Built string } }
	}
	Matches []struct {
		Vulnerability struct {
			ID, Severity, DataSource, Description string
			Fix                                   struct {
				State    string
				Versions []string
			}
		}
		Artifact struct {
			Name, Version, Type string
			Locations           []struct{ LayerID string }
		}
	}
}

func (r grypeReport) findings(layers []Layer) []Finding {
	rows := make([]Finding, 0, len(r.Matches))
	seen := make(map[[3]string]bool)
	for _, m := range r.Matches {
		v, a := m.Vulnerability, m.Artifact
		key := [3]string{v.ID, a.Name, a.Version}
		if seen[key] {
			continue
		}
		seen[key] = true
		f := Finding{ID: v.ID, Severity: v.Severity, Package: a.Name, Version: a.Version,
			Kind: a.Type, Fixed: []string{}, Description: v.Description}
		if f.Severity == "" {
			f.Severity = "Unknown"
		}
		if v.Fix.State == "fixed" && len(v.Fix.Versions) > 0 {
			f.Fixed = v.Fix.Versions
		}
		if strings.HasPrefix(v.DataSource, "https://") {
			f.URL = v.DataSource
		}
		for _, loc := range a.Locations {
			if loc.LayerID == "" {
				continue
			}
			i := slices.IndexFunc(layers, func(l Layer) bool { return l.DiffID == loc.LayerID || l.Digest == loc.LayerID })
			if i >= 0 {
				f.Layer, f.By, f.Base = &i, &layers[i].By, &layers[i].Base
				break
			}
		}
		rows = append(rows, f)
	}
	slices.SortStableFunc(rows, func(a, b Finding) int {
		if order := cmp.Compare(severityRank(a.Severity), severityRank(b.Severity)); order != 0 {
			return order
		}
		if (len(a.Fixed) > 0) != (len(b.Fixed) > 0) {
			return cmp.Compare(min(len(b.Fixed), 1), min(len(a.Fixed), 1))
		}
		return cmp.Compare(a.Package, b.Package)
	})
	return rows
}

func severityRank(severity string) int {
	order := [...]string{"Critical", "High", "Medium", "Low", "Negligible", "Unknown"}
	if i := slices.Index(order[:], severity); i >= 0 {
		return i
	}
	return len(order)
}
