package main

import (
	"fmt"
	"strings"
	"testing"
	"time"

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

// setShareBillingFixture supplies the plan and tunnel states used by tests and screenshots.
func setShareBillingFixture(f *fakeEngine, state string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	id, name, tunnels, used, cap := "free", "免费版", 1, uint64(420*1024*1024), uint64(1<<30)
	if state == "paid" {
		id, name, tunnels, used, cap = "basic", "基础版", 5, 12<<30, 50<<30
	}
	if state == "warning" {
		used = cap * 85 / 100
	}
	if state == "quota" {
		used = cap
	}
	expires := "null"
	if state == "paid" {
		expires = `"2027-01-15T00:00:00+08:00"`
	}
	history := fmt.Sprintf(`[{"month":"2026-06","used":%d},{"month":"2026-07","used":%d},{"month":"2026-08","used":%d},{"month":"2026-09","used":0},{"month":"2026-10","used":%d},{"month":"2026-11","used":%d}]`, cap/3, cap/2, cap/5, cap*3/4, used)
	f.billing = fmt.Sprintf(`{"billing":true,"plan":{"id":%q,"name":%q,"expires":%s},"limits":{"tunnels":%d},"usage":{"used":%d,"cap":%d,"resets":"2026-12-01T00:00:00+08:00","history":%s}}`, id, name, expires, tunnels, used, cap, history)
	f.share = fakeShare(time.Now().Unix())
	if state == "quota" {
		f.share = strings.ReplaceAll(f.share, `"state":"live"`, `"state":"waiting","reason":"quota exhausted: monthly traffic"`)
		f.share = strings.ReplaceAll(f.share, `"state":"reconnecting"`, `"state":"waiting"`)
	}
}

// TestShareBilling checks account refresh cadence, background reads, and the legacy fallback.
func TestShareBilling(t *testing.T) {
	f, s := newFakeEngine(t)
	setShareBillingFixture(f, "free")
	tt := ui.NewTester(s.view, 1180, 760)
	click(t, tt, "Share")
	tt.Frame()
	if m := has(tt, "Free", "Upgrade", "420 MB / 1.0 GB"); m != "" {
		t.Fatalf("missing %q: %v", m, tt.Texts())
	}
	s.refresh()
	f.mu.Lock()
	reads, background := f.accountReads, f.accountBackground
	f.mu.Unlock()
	if reads != 1 || !background {
		t.Fatalf("account reads=%d background=%v", reads, background)
	}
	s.shares.billingChecked = time.Now().Add(-time.Minute)
	s.refresh()
	f.mu.Lock()
	reads = f.accountReads
	f.mu.Unlock()
	if reads != 2 {
		t.Fatalf("minute refresh reads=%d", reads)
	}
	s.navigate(containersPage)
	s.shares.billingChecked = time.Time{}
	s.refresh()
	f.mu.Lock()
	reads = f.accountReads
	f.billing = `{"billing":false}`
	f.mu.Unlock()
	if reads != 2 {
		t.Fatalf("off-page reads=%d", reads)
	}
	s.navigate(sharePage)
	s.refresh()
	tt.Frame()
	if tt.HasText("Upgrade") || tt.HasText("Free") {
		t.Fatalf("unbilled UI: %v", tt.Texts())
	}
	click(t, tt, "New link")
	if s.form.kind != "share" {
		t.Fatalf("legacy create: %q", s.form.kind)
	}
}

// TestShareBillingActions checks each billing entry point, limits, failures, and waiting links.
func TestShareBillingActions(t *testing.T) {
	for _, entry := range []string{"Upgrade", "Renew", "Plans and billing…", "limit", "quota"} {
		t.Run(entry, func(t *testing.T) {
			f, s := newFakeEngine(t)
			state := "free"
			if entry == "Renew" {
				state = "paid"
			}
			if entry == "quota" {
				state = "quota"
			}
			setShareBillingFixture(f, state)
			tt := ui.NewTester(s.view, 1180, 760)
			click(t, tt, "Share")
			switch entry {
			case "limit":
				click(t, tt, "New link")
				if !tt.HasText("Free allows 1 simultaneous links") {
					t.Fatalf("limit dialog: %v", tt.Texts())
				}
				click(t, tt, "Cancel")
				if s.form.kind != "" || len(f.changed()) != 0 {
					t.Fatal("cancel created a link")
				}
				click(t, tt, "New link")
				click(t, tt, "Upgrade connection allowance")
			case "Plans and billing…":
				click(t, tt, "Share account")
				click(t, tt, entry)
			case "quota":
				if m := has(tt, "Waiting for quota", "Monthly traffic exhausted; resets 12/1"); m != "" {
					t.Fatalf("missing %q: %v", m, tt.Texts())
				}
				// The banner has a distinct accessible name from the toolbar action.
				click(t, tt, "Upgrade traffic allowance")
			default:
				click(t, tt, entry)
			}
			tt.Frame()
			if got := tt.OpenedURLs(); len(got) != 1 || got[0] != "https://edge.fabo.dev/account?lang=en&ticket=test-ticket" {
				t.Fatalf("opened %v", got)
			}
			if got := f.changed(); len(got) != 1 || got[0] != "POST /fabo/share/billing" {
				t.Fatalf("requests %v", got)
			}
		})
	}
	t.Run("failure", func(t *testing.T) {
		f, s := newFakeEngine(t)
		setShareBillingFixture(f, "free")
		f.mu.Lock()
		f.billingError = true
		f.mu.Unlock()
		tt := ui.NewTester(s.view, 1180, 760)
		click(t, tt, "Share")
		click(t, tt, "Upgrade")
		tt.Frame()
		if len(tt.OpenedURLs()) != 0 || s.working["share-billing"] {
			t.Fatal("failed billing request opened a page or stayed busy")
		}
		if !tt.HasText("Fabo returned HTTP 503") {
			t.Fatalf("failure hidden: %v", tt.Texts())
		}
	})
}

// TestShareBillingLayout keeps quota actions inside the window and clears billing on sign-out.
func TestShareBillingLayout(t *testing.T) {
	f, s := newFakeEngine(t)
	setShareBillingFixture(f, "quota")
	tt := ui.NewTester(s.view, 1180, 760)
	click(t, tt, "Share")
	for _, width := range []float32{1180, 860} {
		tt.SetSize(int(width), 760)
		tt.Frame()
		for _, label := range []string{"Upgrade", "New link", "Upgrade traffic allowance"} {
			rect, ok := tt.Find(label)
			if !ok || rect.X < 0 || rect.X+rect.W > width-16 {
				t.Fatalf("%s at width %.0f: %+v present=%v", label, width, rect, ok)
			}
		}
	}
	f.mu.Lock()
	f.share = `{"account":{"signedIn":false,"server":"edge.fabo.dev"},"shares":[]}`
	f.mu.Unlock()
	s.refresh()
	tt.Frame()
	if s.shares.billing.Billing || tt.HasText("Upgrade") {
		t.Fatal("sign-out retained billing")
	}
}

// TestShareUsageDialog opens traffic details from the toolbar meter and shows per-link traffic.
func TestShareUsageDialog(t *testing.T) {
	f, s := newFakeEngine(t)
	setShareBillingFixture(f, "paid")
	tt := ui.NewTester(s.view, 1180, 760)
	click(t, tt, "Share")
	tt.Frame()
	// The table's traffic column sums both directions of each link.
	if m := has(tt, "Traffic", "182 KB"); m != "" {
		t.Fatalf("missing %q: %v", m, tt.Texts())
	}
	click(t, tt, "Monthly traffic 12.0 GB / 50.0 GB")
	if s.form.kind != "share-usage" {
		t.Fatalf("form %q", s.form.kind)
	}
	if m := has(tt, "Remaining", "38.0 GB", "Resets", "2026-12-01", "Expires", "2027-01-15", "Recent months", "Nov", "Jun", "2 open links: 2 KB in, 180 KB out"); m != "" {
		t.Fatalf("missing %q: %v", m, tt.Texts())
	}
	click(t, tt, "Close")
	if s.form.kind != "" || len(f.changed()) != 0 {
		t.Fatal("closing the traffic dialog changed something")
	}
	click(t, tt, "Monthly traffic 12.0 GB / 50.0 GB")
	click(t, tt, "From traffic details: Renew")
	tt.Frame()
	if got := f.changed(); len(got) != 1 || got[0] != "POST /fabo/share/billing" {
		t.Fatalf("requests %v", got)
	}
}
