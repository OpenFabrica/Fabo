package main

import (
	"net/http"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"
)

func TestKubernetesPollingKeepsRefreshButtonStable(t *testing.T) {
	for tab, name := range []string{"Pods", "Services"} {
		t.Run(name, func(t *testing.T) {
			f, s := newFakeEngine(t)
			k := installKube(t, f)
			s.navigate(kubernetesPage)
			s.kube.tab = tab
			observed := 0
			intercept(f, func(_ http.ResponseWriter, r *http.Request) bool {
				if r.URL.Path == "/containers/fabo-k8s/json" {
					// The request is in flight; the synchronous test runner is
					// waiting for this response, so state is safe to render here.
					observed++
					tt := ui.NewTester(s.view, 1180, 760)
					tt.Frame()
					button, found := tt.Access("Refresh")
					if !found || button.Disabled {
						t.Error("background polling disables the Refresh button")
					}
				}
				return false
			})
			for range 3 {
				s.refresh()
			}
			if observed != 3 || len(k.commands) != 4 {
				t.Fatalf("polling stopped: observed=%d commands=%d", observed, len(k.commands))
			}
		})
	}
}

func TestKubernetesManualRefresh(t *testing.T) {
	for _, failed := range []bool{false, true} {
		name := "success"
		if failed {
			name = "failure"
		}
		t.Run(name, func(t *testing.T) {
			f, s := newFakeEngine(t)
			k := installKube(t, f)
			s.navigate(kubernetesPage)
			if failed {
				k.exit = 1
			}
			// An explicit click must bypass the automatic retry cooldown.
			s.kube.retryAt = time.Now().Add(time.Hour)
			observed := false
			intercept(f, func(_ http.ResponseWriter, r *http.Request) bool {
				if r.URL.Path == "/containers/fabo-k8s/json" {
					observed = true
					tt := ui.NewTester(s.view, 1180, 760)
					tt.Frame()
					button, found := tt.Access("Refresh")
					if !found || !button.Disabled {
						t.Error("manual refresh must disable its button while running")
					}
				}
				return false
			})
			tt := ui.NewTester(s.view, 1180, 760)
			click(t, tt, "Refresh")
			tt.Frame()
			button, found := tt.Access("Refresh")
			if !observed || !found || button.Disabled || s.kube.loading {
				t.Fatalf("manual refresh did not settle: observed=%v button=%+v", observed, button)
			}
			if len(k.commands) != 2 || (s.kube.problem != "") != failed {
				t.Fatalf("unexpected refresh result: commands=%d problem=%q", len(k.commands), s.kube.problem)
			}
		})
	}
}

func TestKubernetesManualRefreshJoinsBackgroundRequest(t *testing.T) {
	f, s := newFakeEngine(t)
	k := installKube(t, f)
	s.navigate(kubernetesPage)
	intercept(f, func(_ http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path == "/containers/fabo-k8s/json" {
			tt := ui.NewTester(s.view, 1180, 760)
			click(t, tt, "Refresh")
			tt.Frame()
			button, found := tt.Access("Refresh")
			if !found || !button.Disabled {
				t.Error("manual refresh must wait for the existing background request")
			}
		}
		return false
	})
	s.refresh()
	tt := ui.NewTester(s.view, 1180, 760)
	tt.Frame()
	button, found := tt.Access("Refresh")
	if !found || button.Disabled || len(k.commands) != 2 {
		t.Fatalf("refresh did not share and settle the request: button=%+v commands=%d", button, len(k.commands))
	}
}
