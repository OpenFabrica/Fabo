// Package imagescan runs Grype against local OCI images and keeps scan history.
package imagescan

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// The pinned Grype release and the SHA-256 of its archive for each platform.
const version = "0.120.1"

var grypeChecksums = map[string]string{
	"darwin/arm64": "cf97957fa467d25575ec2cc3228289f391ea51cf88b9cffc5f83dc03d4cbc732",
	"darwin/amd64": "5313004ccbc524c8757521dc3913f1edf4309f56bef06a2fb2d0c0eeade7cc62",
	"linux/arm64":  "29f47391dc283aa79fcc38e65224cd61f64dec0ecfd0db7074128ebf8ff23514",
	"linux/amd64":  "0a9ee97ef5ae2ee953b0a80098105052e846cdbe319a57d808b519c33cd1343d",
}

const (
	keepScans     = 20
	maxDownload   = 64 << 20
	maxExecutable = 128 << 20
	reportFile    = "grype.json"
	sbomFile      = "sbom.cdx.json"
	resultFile    = "result.json"
	summaryFile   = "summary.json"
)

// Scanner is shared across the daemon's VM lifecycles.
type Scanner struct {
	root    string
	mu      sync.Mutex
	running map[string]bool
	prepare sync.Mutex
}

func New(root string) *Scanner {
	return &Scanner{root: root, running: make(map[string]bool)}
}

// Scan builds a private OCI layout and publishes the report and SBOM together.
// The image ID is a full config digest resolved by the engine before the scan.
func (s *Scanner) Scan(ctx context.Context, id string, layers []Layer, layout func(string) error,
	client *http.Client, env []string, progress func(string, int) error) (Result, error) {
	dir, err := s.scanDir(id)
	if err != nil {
		return Result{}, err
	}
	s.mu.Lock()
	if s.running[id] {
		s.mu.Unlock()
		return Result{}, fmt.Errorf("this image is being scanned already")
	}
	s.running[id] = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.running, id)
		s.mu.Unlock()
	}()

	if err := os.MkdirAll(dir, 0700); err != nil {
		return Result{}, err
	}
	stage, err := os.MkdirTemp(dir, ".scan-")
	if err != nil {
		return Result{}, err
	}
	defer os.RemoveAll(stage)
	input := filepath.Join(stage, "image")
	if err := layout(input); err != nil {
		return Result{}, fmt.Errorf("prepare image for scanning: %w", err)
	}
	grype, err := s.prepareGrype(ctx, client, env, progress)
	if err != nil {
		return Result{}, err
	}
	if err := progress("scanning", 0); err != nil {
		return Result{}, err
	}
	if err := runGrype(ctx, grype, s.cache(), env, "scan image",
		"oci-dir:"+input,
		"-o", "json="+filepath.Join(stage, reportFile),
		"-o", "cyclonedx-json="+filepath.Join(stage, sbomFile)); err != nil {
		return Result{}, err
	}
	if err := os.RemoveAll(input); err != nil {
		return Result{}, err
	}
	if _, err := os.Stat(filepath.Join(stage, sbomFile)); err != nil {
		return Result{}, fmt.Errorf("scan did not produce an SBOM: %w", err)
	}
	times, err := scanTimes(dir)
	if err != nil {
		return Result{}, err
	}
	at := time.Now().Unix()
	if len(times) > 0 {
		at = max(at, times[0]+1)
	}
	result, err := readReport(stage, id, at, layers)
	if err != nil {
		return Result{}, err
	}
	if err := writeJSON(filepath.Join(stage, resultFile), result); err != nil {
		return Result{}, err
	}
	if err := writeJSON(filepath.Join(stage, summaryFile), result.summary()); err != nil {
		return Result{}, err
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	if err := os.Rename(stage, reportDir(dir, at)); err != nil {
		return Result{}, err
	}
	for _, old := range times[min(len(times), keepScans-1):] {
		_ = os.RemoveAll(reportDir(dir, old))
	}
	return result, nil
}

func (s *Scanner) cache() string { return filepath.Join(s.root, "scanner", "db") }

func (s *Scanner) prepareGrype(ctx context.Context, client *http.Client, env []string,
	progress func(string, int) error) (string, error) {
	s.prepare.Lock()
	defer s.prepare.Unlock()
	if err := ctx.Err(); err != nil {
		return "", err
	}
	dir := filepath.Join(s.root, "scanner")
	grype := filepath.Join(dir, "grype-"+version)
	if info, err := os.Stat(grype); err != nil || !info.Mode().IsRegular() {
		if err := os.MkdirAll(dir, 0700); err != nil {
			return "", err
		}
		if err := downloadGrype(ctx, client, grype, progress); err != nil {
			return "", err
		}
	}
	// The database is refreshed at most once per dbMaxAge; scans in between
	// reuse the cached copy without asking the network.
	stamp := filepath.Join(dir, dbStamp)
	if info, err := os.Stat(stamp); err == nil && time.Since(info.ModTime()) < dbMaxAge {
		return grype, nil
	}
	if err := progress("database", 0); err != nil {
		return "", err
	}
	if err := runGrype(ctx, grype, s.cache(), env, "update vulnerability database", "db", "update"); err != nil {
		if status := runGrype(ctx, grype, s.cache(), env, "open cached vulnerability database", "db", "status"); status != nil {
			return "", err
		}
		// An offline scan uses the cached copy but retries the update next time.
		return grype, nil
	}
	_ = os.WriteFile(stamp, nil, 0600)
	_ = os.Chtimes(stamp, time.Now(), time.Now())
	return grype, nil
}

// dbStamp marks, by its modification time, the last successful database
// update; dbMaxAge is how long that copy is used before updating again.
const (
	dbStamp  = "db-updated"
	dbMaxAge = 24 * time.Hour
)

func runGrype(ctx context.Context, binary, cache string, env []string, action string, args ...string) error {
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Env = append(os.Environ(), env...)
	cmd.Env = append(cmd.Env, "GRYPE_DB_CACHE_DIR="+cache, "GRYPE_CHECK_FOR_APP_UPDATE=false", "GRYPE_DB_AUTO_UPDATE=false")
	_, err := cmd.Output()
	var failure *exec.ExitError
	if !errors.As(err, &failure) {
		return err
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	message := strings.TrimSpace(string(failure.Stderr))
	if newline := strings.LastIndexByte(message, '\n'); newline >= 0 {
		message = strings.TrimSpace(message[newline+1:])
	}
	for _, entry := range env {
		if _, value, ok := strings.Cut(entry, "="); ok && value != "" {
			message = strings.ReplaceAll(message, value, "[proxy]")
		}
	}
	if message == "" {
		message = failure.Error()
	}
	return fmt.Errorf("%s: %s", action, message)
}

func downloadGrype(ctx context.Context, client *http.Client, destination string, progress func(string, int) error) error {
	sum := grypeChecksums[runtime.GOOS+"/"+runtime.GOARCH]
	if sum == "" {
		return fmt.Errorf("Grype is unavailable for %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	if err := progress("scanner", 0); err != nil {
		return err
	}
	url := fmt.Sprintf("https://github.com/anchore/grype/releases/download/v%s/grype_%s_%s_%s.tar.gz",
		version, version, runtime.GOOS, runtime.GOARCH)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("download Grype: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.ContentLength > maxDownload {
		return fmt.Errorf("download Grype: unexpected response %s (%d bytes)", resp.Status, resp.ContentLength)
	}
	dir, err := os.MkdirTemp(filepath.Dir(destination), ".download-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	archive, err := os.Create(filepath.Join(dir, "grype.tar.gz"))
	if err != nil {
		return err
	}
	defer archive.Close()
	hash := sha256.New()
	reader := &downloadProgress{reader: io.LimitReader(resp.Body, maxDownload+1), total: resp.ContentLength, progress: progress}
	n, err := io.Copy(io.MultiWriter(archive, hash), reader)
	if err != nil {
		return err
	}
	if n > maxDownload || fmt.Sprintf("%x", hash.Sum(nil)) != sum {
		return fmt.Errorf("Grype download checksum mismatch")
	}
	if _, err := archive.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if err := extractGrype(archive, filepath.Join(dir, "grype")); err != nil {
		return err
	}
	if err := progress("scanner", 100); err != nil {
		return err
	}
	return os.Rename(filepath.Join(dir, "grype"), destination)
}

type downloadProgress struct {
	reader      io.Reader
	total, read int64
	pct         int
	progress    func(string, int) error
}

func (p *downloadProgress) Read(buf []byte) (int, error) {
	n, err := p.reader.Read(buf)
	p.read += int64(n)
	if p.total > 0 {
		pct := min(99, int(p.read*100/p.total))
		if pct >= p.pct+5 {
			p.pct = pct
			if err := p.progress("scanner", pct); err != nil {
				return n, err
			}
		}
	}
	return n, err
}

func extractGrype(archive io.Reader, destination string) error {
	gz, err := gzip.NewReader(archive)
	if err != nil {
		return err
	}
	defer gz.Close()
	tarball := tar.NewReader(gz)
	for {
		h, err := tarball.Next()
		if err != nil {
			return fmt.Errorf("unpack Grype: %w", err)
		}
		if h.Name != "grype" {
			continue
		}
		if h.Typeflag != tar.TypeReg || h.Size <= 0 || h.Size > maxExecutable {
			return fmt.Errorf("invalid Grype executable in archive")
		}
		f, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0700)
		if err != nil {
			return err
		}
		_, err = io.Copy(f, tarball)
		return errors.Join(err, f.Close())
	}
}
