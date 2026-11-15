package main

import (
	"strings"
	"testing"

	"github.com/egoist/mygo/ui"
)

// The Share page lists the engine's links with their state, opens a new
// one, and stops one.
func TestSharePage(t *testing.T) {
	f, s := newFakeEngine(t)
	tt := ui.NewTester(s.view, 1180, 760)
	click(t, tt, "Share")
	tt.Frame()
	if m := has(tt, "mike", "edge.fabo.dev", "shop-web-1", "https://shop-web-1-x7k2ab.fabo.link", "Live", "localhost:5173", "Connecting"); m != "" {
		t.Fatalf("missing %q in %q", m, tt.Texts())
	}
	click(t, tt, "New link")
	if s.form.kind != "share" {
		t.Fatalf("form %q", s.form.kind)
	}
	// An empty target is refused in place. (Enter submits; "Share" is
	// also the sidebar's label.)
	tt.Key(0, ui.KeyEnter)
	if s.form.problem == "" {
		t.Fatal("an empty target was accepted")
	}
	tt.Type("3000")
	tt.Key(0, ui.KeyEnter)
	if got := f.changed(); len(got) != 1 || got[0] != "POST /fabo/share" || !strings.Contains(f.last, `"target":"3000"`) {
		t.Fatalf("create: %v %s", got, f.last)
	}
	click(t, tt, "Stop sharing shop-web-1")
	if got := f.changed(); len(got) != 2 || got[1] != "DELETE /fabo/share/sh-web" {
		t.Fatalf("stop: %v", got)
	}
	// The panel shows a link's requests, failures included.
	s.selectRow("sh-web")
	s.tab[sharePage] = 1
	tt.Frame()
	if m := has(tt, "/assets/app.js", "502"); m != "" {
		t.Fatalf("missing %q in %q", m, tt.Texts())
	}
}

// Without a relay the page asks for one; signed out it offers sign-in, and
// a token signs in through the engine.
func TestShareFirstRun(t *testing.T) {
	f, s := newFakeEngine(t)
	f.mu.Lock()
	f.share = `{"account":{"signedIn":false,"server":""},"shares":[]}`
	f.mu.Unlock()
	tt := ui.NewTester(s.view, 1180, 760)
	click(t, tt, "Share")
	tt.Frame()
	click(t, tt, "Set relay")
	if s.form.kind != "share-relay" {
		t.Fatalf("form %q", s.form.kind)
	}
	tt.Type("edge.example.com")
	click(t, tt, "Save")
	if got := f.changed(); len(got) != 1 || got[0] != "POST /fabo/share/login" || !strings.Contains(f.last, `"server":"edge.example.com"`) {
		t.Fatalf("relay: %v %s", got, f.last)
	}
	f.mu.Lock()
	f.share = `{"account":{"signedIn":false,"server":"edge.example.com"},"shares":[]}`
	f.mu.Unlock()
	s.refreshing = false
	s.refresh()
	tt.Frame()
	if !tt.HasText("Sign in to publish a port or container") {
		t.Fatalf("signed-out page: %q", tt.Texts())
	}
	click(t, tt, "Sign in")
	if s.form.kind != "share-login" {
		t.Fatalf("form %q", s.form.kind)
	}
	tt.Type("ft_secret")
	tt.Key(0, ui.KeyEnter)
	if got := f.changed(); len(got) != 2 || !strings.Contains(f.last, `"token":"ft_secret"`) {
		t.Fatalf("token login: %v %s", got, f.last)
	}
	// Changing the relay is offered from the account menu.
	s.form = form{}
	tt.Frame()
	click(t, tt, "Share account")
	if err := tt.Click("Change relay…"); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	if s.form.kind != "share-relay" || s.form.host != "edge.example.com" {
		t.Fatalf("change relay: %+v", s.form)
	}
}

// The GitHub sign-in shows the code while the engine waits, and closes once
// the account is signed in.
func TestShareGitHubSignIn(t *testing.T) {
	f, s := newFakeEngine(t)
	f.mu.Lock()
	f.share = `{"account":{"signedIn":false,"server":"edge.example.com","pending":{"code":"ABCD-1234","uri":"https://github.com/login/device"}},"shares":[]}`
	f.mu.Unlock()
	tt := ui.NewTester(s.view, 1180, 760)
	click(t, tt, "Share")
	tt.Frame()
	click(t, tt, "Sign in")
	click(t, tt, "Sign in with GitHub")
	tt.Frame()
	if !strings.Contains(f.last, `"github":true`) || !tt.HasText("ABCD-1234") || !tt.HasText("Waiting for approval…") {
		t.Fatalf("device code: %s %q", f.last, tt.Texts())
	}
	f.mu.Lock()
	f.share = `{"account":{"signedIn":true,"login":"octo","server":"edge.example.com"},"shares":[]}`
	f.mu.Unlock()
	s.refreshing = false
	s.refresh()
	// The view turns notices into toasts, so look before the next frame.
	if s.form.kind != "" || !strings.Contains(strings.Join(s.notices, " "), "Signed in as octo") {
		t.Fatalf("after approval: form %q, notices %q", s.form.kind, s.notices)
	}
}
