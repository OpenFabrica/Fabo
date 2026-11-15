//go:build linux || darwin

package localengine

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/OpenFabrica/Fabo/engine/internal/shareedge"
	"github.com/OpenFabrica/Fabo/engine/internal/shareedge/edgetest"
	"github.com/OpenFabrica/Fabo/engine/internal/sharetunnel"
)

// TestBillingReadAndPaymentWatchRetryWaitingLinks exercise HTTP and polling recovery against a real relay.
func TestBillingReadAndPaymentWatchRetryWaitingLinks(t *testing.T) {
	for _, poll := range []bool{false, true} {
		t.Run(map[bool]string{false: "account", true: "payment watch"}[poll], func(t *testing.T) {
			h := newShareHarness(t, shareedge.Config{Billing: &shareedge.BillingConfig{Plans: []shareedge.Plan{{ID: "free", Name: "Free", Tunnels: 1, Names: 1, Traffic: 1 << 20}}}}, PublicShareOptions{})
			h.signIn("billing")
			var first, second struct{ ID string }
			if h.call("POST", "/fabo/share", `{"target":"3000","noReserve":true}`, &first) != 201 {
				t.Fatal("first create")
			}
			h.waitFor("first live", func(r shareReport) bool { return len(r.Shares) == 1 && r.Shares[0].State == "live" })
			if h.call("POST", "/fabo/share", `{"target":"3001","noReserve":true}`, &second) != 201 {
				t.Fatal("second create")
			}
			h.waitFor("quota waiting", func(r shareReport) bool { return len(r.Shares) == 2 && r.Shares[1].State == "waiting" })
			h.call("DELETE", "/fabo/share/"+first.ID, "", nil)
			if poll {
				var out struct{ URL string }
				if h.call("POST", "/fabo/share/billing", `{}`, &out) != 200 || !strings.Contains(out.URL, "/account?t=") {
					t.Fatalf("billing: %+v", out)
				}
				h.shares.mu.Lock()
				api, err := h.shares.apiLocked()
				started := h.shares.billingCancel != nil
				h.shares.mu.Unlock()
				if err != nil || !started {
					t.Fatal("billing route did not start watch")
				}
				h.shares.startBillingPoll(api, 10*time.Millisecond, time.Second)
			} else {
				if h.call("GET", "/fabo/share/account", "", nil) != 200 {
					t.Fatal("account read")
				}
			}
			h.waitFor("recovered", func(r shareReport) bool {
				return len(r.Shares) == 1 && r.Shares[0].ID == second.ID && r.Shares[0].State == "live"
			})
		})
	}
}

// TestBillingPollLifetimeAndCancellation verifies timeout and account changes cancel even a blocked network call.
func TestBillingPollLifetimeAndCancellation(t *testing.T) {
	for _, stop := range []string{"timeout", "logout", "relay", "close"} {
		t.Run(stop, func(t *testing.T) {
			ca := edgetest.NewCA(t)
			entered, cancelled := make(chan struct{}), make(chan struct{})
			var calls atomic.Int32
			srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				close(entered)
				<-r.Context().Done()
				close(cancelled)
			}))
			srv.TLS = &tls.Config{Certificates: []tls.Certificate{ca.Cert}}
			srv.StartTLS()
			defer srv.Close()
			t.Setenv("FABO_SHARE_CA", ca.Path)
			t.Setenv("FABO_SHARE_CONNECT", srv.Listener.Addr().String())
			t.Setenv("FABO_SHARE_SERVER", "")
			t.Setenv("FABO_SHARE_TOKEN", "")
			p, err := OpenPublicShares(PublicShareOptions{ConfigPath: t.TempDir() + "/share.json"})
			if err != nil {
				t.Fatal(err)
			}
			defer p.Close()
			p.cfg.Server = edgetest.EdgeHost
			p.cfg.SetToken("token", "a")
			api, err := p.apiLocked()
			if err != nil {
				t.Fatal(err)
			}
			lifetime := time.Hour
			if stop == "timeout" {
				lifetime = 100 * time.Millisecond
			}
			p.startBillingPoll(api, time.Millisecond, lifetime)
			select {
			case <-entered:
			case <-time.After(time.Second):
				t.Fatal("no poll")
			}
			switch stop {
			case "logout":
				if _, err = p.signOut(); err != nil {
					t.Fatal(err)
				}
			case "relay":
				p.mu.Lock()
				_, err = p.switchServerLocked("another.test")
				p.mu.Unlock()
				if err != nil {
					t.Fatal(err)
				}
			case "close":
				p.Close()
			}
			select {
			case <-cancelled:
			case <-time.After(time.Second):
				t.Fatal("poll I/O outlived watch")
			}
			p.billingWG.Wait()
			if calls.Load() != 1 {
				t.Fatal("cancelled poll kept running")
			}
			// A stale response may not change the new account's cached plan.
			p.observeAccount(api, sharetunnel.Account{Billing: true, Plan: sharetunnel.Plan{ID: "stale"}})
			if stop != "timeout" {
				p.mu.Lock()
				plan := p.lastPlan
				p.mu.Unlock()
				if plan != nil {
					t.Fatal("stale account response accepted")
				}
			}
		})
	}
}

// TestAccountPlanComparisonUsesValues retries an exhausted waiting link only when service actually changes.
func TestAccountPlanComparisonUsesValues(t *testing.T) {
	h := newShareHarness(t, shareedge.Config{Billing: &shareedge.BillingConfig{Plans: []shareedge.Plan{{ID: "free", Name: "Free", Tunnels: 1, Names: 1, Traffic: 1 << 20}}}}, PublicShareOptions{})
	h.signIn("compare")
	var first struct{ ID string }
	h.call("POST", "/fabo/share", `{"target":"3000","noReserve":true}`, &first)
	h.waitFor("first live", func(r shareReport) bool { return len(r.Shares) == 1 && r.Shares[0].State == "live" })
	h.call("POST", "/fabo/share", `{"target":"3001","noReserve":true}`, nil)
	h.waitFor("waiting", func(r shareReport) bool { return len(r.Shares) == 2 && r.Shares[1].State == "waiting" })
	h.call("DELETE", "/fabo/share/"+first.ID, "", nil)
	h.shares.mu.Lock()
	api, err := h.shares.apiLocked()
	h.shares.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	end := time.Now().Add(time.Hour)
	acct := sharetunnel.Account{Billing: true, Plan: sharetunnel.Plan{ID: "free", Expires: &end}}
	cap := uint64(0)
	acct.Usage.Cap = &cap
	h.shares.observeAccount(api, acct)
	copyEnd := end
	acct.Plan.Expires = &copyEnd
	h.shares.observeAccount(api, acct)
	time.Sleep(30 * time.Millisecond)
	if h.report().Shares[0].State != "waiting" {
		t.Fatal("identical expiry value retried exhausted link")
	}
	acct.Plan.ID = "pro"
	h.shares.observeAccount(api, acct)
	h.waitFor("plan recovery", func(r shareReport) bool { return len(r.Shares) == 1 && r.Shares[0].State == "live" })
}
