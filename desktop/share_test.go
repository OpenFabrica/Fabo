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
	if m := has(tt, "mike", "edge.fabo.dev", "1 live", "shop-web-1", "shop-web-1-x7k2ab.fabo.link", "Live", "localhost:5173", "Connecting"); m != "" {
		t.Fatalf("missing %q in %q", m, tt.Texts())
	}
	// A live address opens itself; one that is reconnecting is only text.
	click(t, tt, "Open https://shop-web-1-x7k2ab.fabo.link")
	if got := tt.OpenedURLs(); len(got) != 1 || got[0] != "https://shop-web-1-x7k2ab.fabo.link" {
		t.Fatalf("opened %v", got)
	}
	if _, found := tt.Find("Open https://q8m2zt1c.fabo.link"); found {
		t.Fatal("a reconnecting address is offered as a link")
	}
	click(t, tt, "New link")
	if s.form.kind != "share" || s.form.flag {
		t.Fatalf("form %+v", s.form)
	}
	// Nothing is chosen for the user: Enter without a container is refused
	// in place. ("Share" is also the sidebar's label.)
	tt.Key(0, ui.KeyEnter)
	if s.form.problem == "" || len(f.changed()) != 0 {
		t.Fatalf("an empty target was accepted: %v", f.changed())
	}
	// A port of this machine is the other kind; typing starts in its field.
	click(t, tt, "Local port")
	if !s.form.flag || s.form.problem != "" {
		t.Fatalf("after choosing the port: %+v", s.form)
	}
	tt.Key(0, ui.KeyEnter)
	if s.form.problem == "" {
		t.Fatal("an empty port was accepted")
	}
	tt.Type("3000")
	tt.Key(0, ui.KeyEnter)
	if got := f.changed(); len(got) != 1 || got[0] != "POST /fabo/share" || !strings.Contains(f.last, `"target":"3000"`) {
		t.Fatalf("create: %v %s", got, f.last)
	}
	// Stopping a link that has an address asks first, and Cancel keeps it.
	click(t, tt, "Stop sharing shop-web-1")
	if s.pending == nil || len(f.changed()) != 1 {
		t.Fatalf("stop did not ask: %v", f.changed())
	}
	click(t, tt, "Cancel")
	if s.pending != nil || len(f.changed()) != 1 {
		t.Fatalf("cancel still stopped: %v", f.changed())
	}
	click(t, tt, "Stop sharing shop-web-1")
	click(t, tt, "Stop sharing")
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

// A container's row shares it in two steps: its button opens New link with
// the container chosen and typing at the name, and Enter sends it. The link
// is announced once it is live, with a button that copies its address.
func TestContainerQuickShare(t *testing.T) {
	f, s := newFakeEngine(t)
	tt := ui.NewTester(s.view, 1180, 760)
	tt.Frame()
	// Only running containers can be shared.
	s.filter[containersPage] = 0
	tt.Frame()
	if _, found := tt.Find("Share loki"); found {
		t.Fatal("a stopped container offers sharing")
	}
	click(t, tt, "Share shop-api-1")
	if s.form.kind != "share" || s.form.host != "shop-api-1" || s.form.flag {
		t.Fatalf("form %+v", s.form)
	}
	tt.Type("api")
	tt.Key(0, ui.KeyEnter)
	if got := f.changed(); len(got) != 1 || got[0] != "POST /fabo/share" ||
		!strings.Contains(f.last, `"target":"shop-api-1"`) || !strings.Contains(f.last, `"name":"api"`) {
		t.Fatalf("create: %v %s", got, f.last)
	}
	// The list read right after does not show the link yet; it stays watched.
	if _, watched := s.shares.watch["sh-new"]; !watched {
		t.Fatalf("the new link is not watched: %v", s.shares.watch)
	}
	// Connecting, nothing is said yet.
	connecting := strings.Replace(fakeShare(time.Now().Unix()), `"shares":[`,
		`"shares":[{"id":"sh-new","target":"shop-api-1","name":"api","owner":"app","state":"connecting","started":"2026-11-16T00:00:00Z","requests":0,"recent":[]},`, 1)
	f.mu.Lock()
	f.share = connecting
	f.mu.Unlock()
	s.refresh()
	tt.Frame()
	if tt.HasText("shop-api-1 is live") || !s.shares.watch["sh-new"] {
		t.Fatalf("announced before live: %q", tt.Texts())
	}
	f.mu.Lock()
	f.share = strings.Replace(connecting, `"state":"connecting"`, `"url":"https://api.fabo.link","state":"live"`, 1)
	f.mu.Unlock()
	s.refresh()
	tt.Frame()
	if !tt.HasText("shop-api-1 is live") || len(s.shares.watch) != 0 {
		t.Fatalf("live link not announced once: %q %v", tt.Texts(), s.shares.watch)
	}
	click(t, tt, "Copy link")
	tt.Frame()
	if !tt.HasText("Copied api.fabo.link") {
		t.Fatalf("copy not confirmed: %q", tt.Texts())
	}
	// A shared container's button is its link's menu.
	if _, found := tt.Find("Share shop-api-1"); found {
		t.Fatal("a shared container still offers a new link")
	}
	s.selectRow("c-redis")
	click(t, tt, "Public link of shop-web-1")
	click(t, tt, "Stop sharing")
	if s.pending == nil {
		t.Fatal("stopping from the menu did not ask")
	}
	click(t, tt, "Stop sharing")
	if got := f.changed(); len(got) != 2 || got[1] != "DELETE /fabo/share/sh-web" {
		t.Fatalf("stop: %v", got)
	}
	// Stopping from here leaves this page's own selection alone.
	if s.selected[containersPage] != "c-redis" {
		t.Fatalf("selection moved to %q", s.selected[containersPage])
	}
	click(t, tt, "Public link of shop-web-1")
	click(t, tt, "Share details")
	if s.page != sharePage || s.selected[sharePage] != "sh-web" {
		t.Fatalf("details: page %q, selected %q", s.page, s.selected[sharePage])
	}
}

// A link the relay will not serve is reported with its reason, and one it
// closed can be opened again in its place.
func TestShareFailureAndRetry(t *testing.T) {
	f, s := newFakeEngine(t)
	tt := ui.NewTester(s.view, 1180, 760)
	click(t, tt, "Share")
	tt.Frame()
	s.shares.watch = map[string]bool{"sh-dev": true}
	f.mu.Lock()
	f.share = strings.Replace(fakeShare(time.Now().Unix()), `"state":"reconnecting","reason":"the relay stopped answering"`, `"state":"ended","error":true,"reason":"name already in use"`, 1)
	f.mu.Unlock()
	s.refresh()
	if got := strings.Join(s.notices, "|"); !strings.Contains(got, "localhost:5173: Closed · name already in use") || len(s.shares.watch) != 0 {
		t.Fatalf("failure not reported: %q %v", got, s.shares.watch)
	}
	tt.Frame()
	if _, found := tt.Find("Stop sharing localhost:5173"); found {
		t.Fatal("a closed link still offers stop")
	}
	click(t, tt, "Reconnect localhost:5173")
	if got := f.changed(); len(got) != 2 || got[0] != "DELETE /fabo/share/sh-dev" || got[1] != "POST /fabo/share" || !strings.Contains(f.last, `"target":"localhost:5173"`) {
		t.Fatalf("retry: %v %s", got, f.last)
	}
}

// The containers page does not read the account, so a plan's limit is only
// known once New link is open; sending the form then meets it.
func TestContainerShareMeetsPlanLimit(t *testing.T) {
	f, s := newFakeEngine(t)
	setShareBillingFixture(f, "free")
	tt := ui.NewTester(s.view, 1180, 760)
	tt.Frame()
	if s.shares.billing.Billing {
		t.Fatal("the containers page read the account")
	}
	click(t, tt, "Share shop-api-1")
	if s.form.kind != "share" || !s.shares.billing.Billing {
		t.Fatalf("form %q, billing read %v", s.form.kind, s.shares.billing.Billing)
	}
	tt.Key(0, ui.KeyEnter)
	tt.Frame()
	if s.form.kind != "share-limit" || len(f.changed()) != 0 || !tt.HasText("Free allows 1 simultaneous links") {
		t.Fatalf("limit not met: form %q, requests %v", s.form.kind, f.changed())
	}
}

// Signed out, a container's share button leads to the sign-in.
func TestContainerShareSignedOut(t *testing.T) {
	f, s := newFakeEngine(t)
	f.mu.Lock()
	f.share = `{"account":{"signedIn":false,"server":"edge.example.com"},"shares":[]}`
	f.mu.Unlock()
	s.refresh()
	tt := ui.NewTester(s.view, 1180, 760)
	tt.Frame()
	click(t, tt, "Share shop-web-1")
	if s.form.kind != "share-login" {
		t.Fatalf("form %q", s.form.kind)
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
	if !tt.HasText("Public sharing is in preview; access is by request") {
		t.Fatalf("signed-out page: %q", tt.Texts())
	}
	click(t, tt, "Request or sign in")
	if s.form.kind != "share-login" {
		t.Fatalf("form %q", s.form.kind)
	}
	// Access is asked for by starring the repository and writing a letter;
	// the window offers no self-service sign-in.
	if _, found := tt.Find("Sign in with GitHub"); found {
		t.Fatal("the dialog still offers GitHub sign-in")
	}
	click(t, tt, "Star on GitHub")
	click(t, tt, "Write email")
	if got := tt.OpenedURLs(); len(got) != 2 || got[0] != repoURL || !strings.HasPrefix(got[1], "mailto:"+contactEmail+"?") || strings.Contains(got[1], "+") {
		t.Fatalf("opened %v", got)
	}
	// The buttons took the focus; opening the dialog again puts it back in
	// the token field.
	s.form = form{}
	tt.Frame()
	click(t, tt, "Request or sign in")
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
