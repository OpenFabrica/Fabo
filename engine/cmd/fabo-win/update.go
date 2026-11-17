package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Updates on Windows: the newest release, downloaded, checked and installed,
// as upstream Kiri's crates/kiri-win/src/update.rs does.
//
// The release feed is one small JSON file, {"version", "url", "sha256",
// "size", "notes_url"}, at updateURL, stamped at build time from
// FABO_UPDATE_URL (scripts/windows-bundle.sh); a build without one has no
// updater. `url` is the MSI. The download is checked against the SHA-256,
// then the MSI itself is read (msi.dll) and must carry Fabo's UpgradeCode,
// this build's platform and the version the feed promised, newer than this
// build, before it is staged in %LOCALAPPDATA%\Fabo\update. Installing
// hands the staged MSI to msiexec once fabo-win has quit (the files it
// replaces are in use until then): fabo.ps1 -Update waits for that, runs
// the per-user upgrade silently and opens the new Fabo.
//
// What this cannot do: pin the download to a signing identity, since the
// MSI is not signed yet. The feed is fetched over HTTPS and the file must
// match its SHA-256 from there.

// updateURL is the feed, stamped by scripts/windows-bundle.sh.
var updateURL = ""

// upgradeCode is the MSI's UpgradeCode (installer/windows/fabo.wxs). The x64
// and arm64 MSIs share it; the platform check keeps an update from
// swapping architectures.
const upgradeCode = "{839A68B8-98E7-4BBD-B1DE-DAB0B047B7D4}"

// maxDownload bounds an MSI download.
const maxDownload = 2 << 30

// feedRelease is a feed entry.
type feedRelease struct {
	Version  string `json:"version"`
	URL      string `json:"url"`
	SHA256   string `json:"sha256"`
	Size     int64  `json:"size"`
	NotesURL string `json:"notes_url"`
}

// updateView is GET /update: the window's update card.
type updateView struct {
	// State is "unavailable" (no feed in this build), "idle", "checking",
	// "latest", "available", "downloading", "ready" or "error".
	State    string `json:"state"`
	Current  string `json:"current"`
	Latest   string `json:"latest,omitempty"`
	NotesURL string `json:"notesUrl,omitempty"`
	Done     int64  `json:"done,omitempty"`
	Total    int64  `json:"total,omitempty"`
	Error    string `json:"error,omitempty"`
}

// stagedUpdate is update\staged.json: an MSI checked and ready to install.
type stagedUpdate struct {
	Version string `json:"version"`
	MSI     string `json:"msi"`
	// Attempted: a helper was started for it once. A helper that failed
	// does not make every later launch quit for it again.
	Attempted bool `json:"attempted,omitempty"`
}

// updateSystem is what the updater does to Windows, so it can be tested.
type updateSystem interface {
	// checkMSI verifies the MSI's UpgradeCode, platform and version.
	checkMSI(path, version string) error
	// startHelper starts fabo.ps1 -Update for the staged MSI; it waits for
	// this process to end.
	startHelper(msi string) error
	logf(format string, args ...any)
	quit()
}

// updater checks for, downloads and installs new versions.
type updater struct {
	sys     updateSystem
	dir     string
	feed    string
	current string
	http    *http.Client

	mu     sync.Mutex
	view   updateView
	found  *feedRelease
	cancel context.CancelFunc
}

// newUpdaterFor returns an updater keeping its files under dir.
func newUpdaterFor(sys updateSystem, dir, feed, current string) *updater {
	u := &updater{sys: sys, dir: dir, feed: feed, current: current, http: &http.Client{}}
	u.view = updateView{State: "idle", Current: current}
	if feed == "" {
		u.view.State = "unavailable"
	}
	if st, ok := u.staged(); ok {
		u.view.State, u.view.Latest = "ready", st.Version
	}
	return u
}

// newer reports whether candidate is a later version than current
// (major.minor.patch, numerically; a pre-release suffix is ignored).
func newer(candidate, current string) bool {
	parse := func(v string) [3]int {
		v = strings.TrimPrefix(v, "v")
		v, _, _ = strings.Cut(v, "-")
		var out [3]int
		for i, part := range strings.SplitN(v, ".", 3) {
			n, _ := strconv.Atoi(part)
			out[i] = n
		}
		return out
	}
	a, b := parse(candidate), parse(current)
	for i := range a {
		if a[i] != b[i] {
			return a[i] > b[i]
		}
	}
	return false
}

// platformMatches reports whether an MSI's Template summary property
// ("x64;1033", "Arm64;1033") is for arch ("amd64" or "arm64").
func platformMatches(template, arch string) bool {
	platform, _, _ := strings.Cut(template, ";")
	switch strings.ToLower(platform) {
	case "x64", "amd64":
		return arch == "amd64"
	case "arm64":
		return arch == "arm64"
	}
	return false
}

// allowedURL is an https URL; the feed and the MSI come from nowhere else.
func allowedURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return fmt.Errorf("refusing a non-HTTPS update address: %q", raw)
	}
	return nil
}

// stageDir is where a downloaded MSI waits to be installed.
func (u *updater) stageDir() string { return filepath.Join(u.dir, "update") }

// staged is the MSI waiting to be installed, if it is still there and
// newer than this build.
func (u *updater) staged() (stagedUpdate, bool) {
	var st stagedUpdate
	b, err := os.ReadFile(filepath.Join(u.stageDir(), "staged.json"))
	if err != nil || json.Unmarshal(b, &st) != nil || st.MSI == "" || !newer(st.Version, u.current) {
		return stagedUpdate{}, false
	}
	if _, err := os.Stat(st.MSI); err != nil {
		return stagedUpdate{}, false
	}
	return st, true
}

// saveStaged writes staged.json.
func (u *updater) saveStaged(st stagedUpdate) error {
	b, _ := json.Marshal(st)
	return os.WriteFile(filepath.Join(u.stageDir(), "staged.json"), b, 0o644)
}

// state is GET /update.
func (u *updater) state() updateView {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.view
}

// set changes the view under the lock.
func (u *updater) set(f func(v *updateView)) {
	u.mu.Lock()
	f(&u.view)
	u.mu.Unlock()
}

// action is POST /update/{action}: check, download, cancel, install.
func (u *updater) action(name string) error {
	if u.feed == "" && name != "install" {
		return errors.New("this build has no update feed")
	}
	switch name {
	case "check":
		go u.check()
		return nil
	case "download":
		return u.download()
	case "cancel":
		u.mu.Lock()
		if u.cancel != nil {
			u.cancel()
		}
		u.mu.Unlock()
		return nil
	case "install":
		return u.installNow()
	}
	return errUnknownAction
}

// check reads the feed and records whether a newer version is there.
func (u *updater) check() {
	u.mu.Lock()
	if s := u.view.State; s == "checking" || s == "downloading" || s == "ready" {
		u.mu.Unlock()
		return
	}
	u.view.State, u.view.Error = "checking", ""
	u.mu.Unlock()
	rel, err := u.latest()
	u.set(func(v *updateView) {
		switch {
		case err != nil:
			v.State, v.Error = "error", err.Error()
		case newer(rel.Version, u.current):
			v.State, v.Latest, v.NotesURL, v.Total = "available", rel.Version, rel.NotesURL, rel.Size
			u.found = &rel
		default:
			v.State, v.Latest = "latest", rel.Version
		}
	})
}

// latest fetches and checks the feed.
func (u *updater) latest() (feedRelease, error) {
	var rel feedRelease
	if err := allowedURL(u.feed); err != nil {
		return rel, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.feed, nil)
	if err != nil {
		return rel, err
	}
	resp, err := u.http.Do(req)
	if err != nil {
		return rel, fmt.Errorf("could not reach the update feed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return rel, fmt.Errorf("the update feed answered %s", resp.Status)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&rel); err != nil {
		return rel, errors.New("the update feed is not valid JSON")
	}
	if rel.Version == "" || len(rel.SHA256) != 64 {
		return rel, errors.New("the update feed has no version or SHA-256")
	}
	if _, err := hex.DecodeString(rel.SHA256); err != nil {
		return rel, errors.New("the update feed's SHA-256 is not hex")
	}
	if err := allowedURL(rel.URL); err != nil {
		return rel, err
	}
	return rel, nil
}

// download fetches the found release in the background.
func (u *updater) download() error {
	u.mu.Lock()
	rel := u.found
	if rel == nil || u.view.State != "available" && u.view.State != "error" {
		u.mu.Unlock()
		return errBusy
	}
	ctx, cancel := context.WithCancel(context.Background())
	u.cancel = cancel
	u.view.State, u.view.Error, u.view.Done = "downloading", "", 0
	u.mu.Unlock()
	go func() {
		defer cancel()
		err := u.fetch(ctx, *rel)
		u.set(func(v *updateView) {
			switch {
			case ctx.Err() != nil:
				v.State = "available"
			case err != nil:
				v.State, v.Error = "error", err.Error()
			default:
				v.State = "ready"
			}
		})
		if err != nil {
			u.sys.logf("update: %v", err)
		}
	}()
	return nil
}

// fetch downloads, checks and stages rel.
func (u *updater) fetch(ctx context.Context, rel feedRelease) error {
	if err := os.MkdirAll(u.stageDir(), 0o755); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rel.URL, nil)
	if err != nil {
		return err
	}
	resp, err := u.http.Do(req)
	if err != nil {
		return fmt.Errorf("downloading the update failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("downloading the update failed: %s", resp.Status)
	}
	msi := filepath.Join(u.stageDir(), "Fabo-"+rel.Version+".msi")
	part := msi + ".part"
	f, err := os.Create(part)
	if err != nil {
		return err
	}
	defer os.Remove(part)
	h := sha256.New()
	var got int64
	buf := make([]byte, 256<<10)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			got += int64(n)
			if got > maxDownload {
				f.Close()
				return errors.New("the update is larger than any Fabo installer")
			}
			h.Write(buf[:n])
			if _, err := f.Write(buf[:n]); err != nil {
				f.Close()
				return err
			}
			done := got
			u.set(func(v *updateView) { v.Done = done })
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			f.Close()
			return fmt.Errorf("downloading the update failed: %w", rerr)
		}
	}
	if err := f.Close(); err != nil {
		return err
	}
	if sum := hex.EncodeToString(h.Sum(nil)); !strings.EqualFold(sum, rel.SHA256) {
		return fmt.Errorf("the update did not match its SHA-256 (got %s)", sum)
	}
	if err := u.sys.checkMSI(part, rel.Version); err != nil {
		return err
	}
	if err := os.Rename(part, msi); err != nil {
		return err
	}
	return u.saveStaged(stagedUpdate{Version: rel.Version, MSI: msi})
}

// installNow is "Restart now": the helper waits for fabo-win to quit and
// installs the staged MSI.
func (u *updater) installNow() error {
	st, ok := u.staged()
	if !ok {
		return errors.New("no update is ready to install")
	}
	if err := u.installStaged(st); err != nil {
		return err
	}
	u.sys.quit()
	return nil
}

// installStagedAtLaunch installs an update staged earlier that no helper
// has tried yet; true when this process should exit for it.
func (u *updater) installStagedAtLaunch() bool {
	st, ok := u.staged()
	if !ok || st.Attempted {
		return false
	}
	if err := u.installStaged(st); err != nil {
		u.sys.logf("update: the staged update did not install: %v", err)
		return false
	}
	return true
}

// installStagedAtQuit installs a staged update as fabo-win quits (the
// window's Quit after "Later").
func (u *updater) installStagedAtQuit() {
	if st, ok := u.staged(); ok && !st.Attempted {
		if err := u.installStaged(st); err != nil {
			u.sys.logf("update: the staged update did not install: %v", err)
		}
	}
}

// installStaged checks the staged MSI again (the folder is the user's,
// writable between the download and now) and starts its helper once.
func (u *updater) installStaged(st stagedUpdate) error {
	if err := u.sys.checkMSI(st.MSI, st.Version); err != nil {
		return err
	}
	st.Attempted = true
	if err := u.saveStaged(st); err != nil {
		return err
	}
	return u.sys.startHelper(st.MSI)
}

// loop checks for updates soon after launch and every six hours. A
// development build (0.0.0-dev) never checks by itself.
func (u *updater) loop() {
	if u.feed == "" || strings.HasPrefix(u.current, "0.0.0") {
		return
	}
	time.Sleep(30 * time.Second)
	for {
		u.check()
		time.Sleep(6 * time.Hour)
	}
}
