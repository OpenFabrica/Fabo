package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeUpdateSystem stands in for msi.dll and the helper.
type fakeUpdateSystem struct {
	mu      sync.Mutex
	bad     error
	helpers []string
	quits   int
}

func (f *fakeUpdateSystem) checkMSI(path, version string) error { return f.bad }
func (f *fakeUpdateSystem) startHelper(msi string) error {
	f.mu.Lock()
	f.helpers = append(f.helpers, filepath.Base(msi))
	f.mu.Unlock()
	return nil
}
func (f *fakeUpdateSystem) logf(string, ...any) {}
func (f *fakeUpdateSystem) quit()               { f.quits++ }

// waitState waits until the updater leaves the transient states.
func waitState(t *testing.T, u *updater) updateView {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		v := u.state()
		if v.State != "checking" && v.State != "downloading" {
			return v
		}
		if time.Now().After(deadline) {
			t.Fatalf("stuck in %s", v.State)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// feedServer serves a feed and an MSI over TLS, as a release host would.
func feedServer(t *testing.T, version string, msi []byte, sum string) *httptest.Server {
	var srv *httptest.Server
	srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/latest-windows-x86_64.json":
			fmt.Fprintf(w, `{"version":%q,"url":%q,"sha256":%q,"size":%d}`, version, srv.URL+"/Fabo.msi", sum, len(msi))
		case "/Fabo.msi":
			w.Write(msi)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestUpdateFlow checks, downloads, stages and installs a newer version.
func TestUpdateFlow(t *testing.T) {
	msi := []byte("pretend MSI")
	h := sha256.Sum256(msi)
	srv := feedServer(t, "0.6.0", msi, hex.EncodeToString(h[:]))
	dir := t.TempDir()
	sys := &fakeUpdateSystem{}
	u := newUpdaterFor(sys, dir, srv.URL+"/latest-windows-x86_64.json", "0.5.2")
	u.http = srv.Client()
	u.check()
	if v := u.state(); v.State != "available" || v.Latest != "0.6.0" {
		t.Fatalf("after check: %+v", v)
	}
	if err := u.action("download"); err != nil {
		t.Fatal(err)
	}
	if v := waitState(t, u); v.State != "ready" {
		t.Fatalf("after download: %+v", v)
	}
	if _, err := os.Stat(filepath.Join(dir, "update", "Fabo-0.6.0.msi")); err != nil {
		t.Fatal(err)
	}
	// A fresh start finds it staged and hands it to the helper once.
	u2 := newUpdaterFor(sys, dir, u.feed, "0.5.2")
	if u2.state().State != "ready" {
		t.Fatalf("restart: %+v", u2.state())
	}
	if !u2.installStagedAtLaunch() || u2.installStagedAtLaunch() {
		t.Fatal("the staged update should be tried exactly once at launch")
	}
	if err := u2.action("install"); err != nil || sys.quits != 1 {
		t.Fatalf("install now: %v, quits %d", err, sys.quits)
	}
	if strings.Join(sys.helpers, ",") != "Fabo-0.6.0.msi,Fabo-0.6.0.msi" {
		t.Fatalf("helpers %v", sys.helpers)
	}
	// Once installed (this build is the new version), nothing is staged.
	if u3 := newUpdaterFor(sys, dir, u.feed, "0.6.0"); u3.state().State != "idle" {
		t.Fatalf("after the upgrade: %+v", u3.state())
	}
}

// TestUpdateRefusals: a wrong hash, a package that is not Fabo's, an older
// version and a plain-HTTP feed are all refused.
func TestUpdateRefusals(t *testing.T) {
	msi := []byte("pretend MSI")
	srv := feedServer(t, "0.6.0", msi, strings.Repeat("ab", 32))
	sys := &fakeUpdateSystem{}
	u := newUpdaterFor(sys, t.TempDir(), srv.URL+"/latest-windows-x86_64.json", "0.5.2")
	u.http = srv.Client()
	u.check()
	u.action("download")
	if v := waitState(t, u); v.State != "error" || !strings.Contains(v.Error, "SHA-256") {
		t.Fatalf("bad hash: %+v", v)
	}

	h := sha256.Sum256(msi)
	srv2 := feedServer(t, "0.6.0", msi, hex.EncodeToString(h[:]))
	sys2 := &fakeUpdateSystem{bad: errors.New("the update is not a Fabo installer")}
	u = newUpdaterFor(sys2, t.TempDir(), srv2.URL+"/latest-windows-x86_64.json", "0.5.2")
	u.http = srv2.Client()
	u.check()
	u.action("download")
	if v := waitState(t, u); v.State != "error" || !strings.Contains(v.Error, "not a Fabo installer") {
		t.Fatalf("foreign MSI: %+v", v)
	}
	if _, ok := u.staged(); ok {
		t.Fatal("foreign MSI staged")
	}

	u = newUpdaterFor(sys, t.TempDir(), srv2.URL+"/latest-windows-x86_64.json", "0.7.0")
	u.http = srv2.Client()
	u.check()
	if v := u.state(); v.State != "latest" {
		t.Fatalf("older release: %+v", v)
	}

	u = newUpdaterFor(sys, t.TempDir(), "http://example.invalid/feed.json", "0.5.2")
	u.check()
	if v := u.state(); v.State != "error" || !strings.Contains(v.Error, "non-HTTPS") {
		t.Fatalf("http feed: %+v", v)
	}

	if v := newUpdaterFor(sys, t.TempDir(), "", "0.5.2").state(); v.State != "unavailable" {
		t.Fatalf("no feed: %+v", v)
	}
}

// TestVersionsAndPlatforms covers the comparisons the checks rely on.
func TestVersionsAndPlatforms(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{{"0.6.0", "0.5.2", true}, {"0.5.10", "0.5.9", true}, {"0.5.2", "0.5.2", false}, {"v1.0.0", "0.9.9", true}, {"0.6.0-rc.1", "0.6.0", false}, {"0.5.1", "0.5.2", false}} {
		if got := newer(c.a, c.b); got != c.want {
			t.Errorf("newer(%s, %s) = %v", c.a, c.b, got)
		}
	}
	if !platformMatches("x64;1033", "amd64") || !platformMatches("Arm64;1033", "arm64") || platformMatches("x64;1033", "arm64") || platformMatches("Intel;1033", "amd64") {
		t.Fatal("platformMatches")
	}
}
