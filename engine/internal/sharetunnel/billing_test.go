package sharetunnel

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/OpenFabrica/Fabo/engine/internal/shareedge"
	"github.com/OpenFabrica/Fabo/engine/internal/sharewire"
)

// quotaCatalog supplies a real one-link relay for recovery tests.
func quotaCatalog() *shareedge.BillingConfig {
	return &shareedge.BillingConfig{Plans: []shareedge.Plan{{ID: "free", Name: "Free", Tunnels: 1, Names: 1, Traffic: 1 << 20}}}
}

// TestQuotaRefusalWaitsAndRetryRecovers keeps the handle alive until the occupied slot is released.
func TestQuotaRefusalWaitsAndRetryRecovers(t *testing.T) {
	_, o := relay(t, shareedge.Config{Billing: quotaCatalog()})
	first, _, _ := open(t, o, "127.0.0.1:1")
	rec := newRecorder()
	o.OnEvent = rec.on
	o.Name = "second"
	h := Start(o)
	defer h.Stop()
	waiting := rec.wait(t, EventWaiting, time.Second)
	if waiting.DelayMS != 300000 || !strings.HasPrefix(waiting.Reason, sharewire.QuotaExhaustedPrefix) {
		t.Fatalf("waiting: %+v", waiting)
	}
	select {
	case <-h.Done():
		t.Fatal("quota refusal ended handle")
	default:
	}
	first.Stop()
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(h.Retry)
	}
	wg.Wait()
	rec.wait(t, EventLive, 2*time.Second)
	h.Retry()
	rec.none(t, EventConnecting, 30*time.Millisecond)
}

// TestQuotaTimerAndStop exercise automatic retry and cancellation without a five-minute wall-clock test.
func TestQuotaTimerAndStop(t *testing.T) {
	_, o := relay(t, shareedge.Config{Billing: quotaCatalog()})
	first, _, _ := open(t, o, "127.0.0.1:1")
	rec := newRecorder()
	o.OnEvent = rec.on
	o.Name = "timer"
	o.quotaRetry = 80 * time.Millisecond
	h := Start(o)
	rec.wait(t, EventWaiting, time.Second)
	first.Stop()
	rec.wait(t, EventLive, time.Second)
	h.Stop()
	rec.wait(t, EventEnded, time.Second)
	// A stopped quota waiter must also close Done immediately.
	first, _, _ = open(t, o, "127.0.0.1:1")
	o.quotaRetry = time.Hour
	rec = newRecorder()
	o.OnEvent = rec.on
	o.Name = "stopped"
	h = Start(o)
	rec.wait(t, EventWaiting, time.Second)
	h.Stop()
	select {
	case <-h.Done():
	case <-time.After(time.Second):
		t.Fatal("Stop left quota waiter running")
	}
}

// TestQuotaGoodbyeAndRefusedClassifyOnlyThePrefix distinguishes billing recovery from permanent refusals.
func TestQuotaGoodbyeAndRefusedClassifyOnlyThePrefix(t *testing.T) {
	s := &session{}
	for _, m := range []sharewire.Msg{sharewire.Refused{Reason: sharewire.QuotaExhaustedPrefix + " limit"}, sharewire.Goodbye{Reason: sharewire.QuotaExhaustedPrefix + " traffic"}} {
		e, done := s.handle(m)
		if !done || e.kind != endWaiting {
			t.Fatalf("quota frame: %+v", e)
		}
	}
	e, _ := s.handle(sharewire.Refused{Reason: "token revoked"})
	if e.kind != endFinal {
		t.Fatal("permanent refusal became retryable")
	}
}

// TestRetryFromWaitingEventIsNotLost exercises the earliest legal wake-up point.
func TestRetryFromWaitingEventIsNotLost(t *testing.T) {
	_, o := relay(t, shareedge.Config{Billing: quotaCatalog()})
	first, _, _ := open(t, o, "127.0.0.1:1")
	rec := newRecorder()
	ready := make(chan struct{})
	var h *Handle
	o.Name = "immediate"
	o.OnEvent = func(e Event) {
		<-ready
		if e.Type == EventWaiting {
			first.Stop()
			h.Retry()
		}
		rec.on(e)
	}
	h = Start(o)
	close(ready)
	defer h.Stop()
	rec.wait(t, EventLive, 2*time.Second)
}

// TestBillingAccountAndTicketAPI preserves legacy defaults and decodes real billed account fields.
func TestBillingAccountAndTicketAPI(t *testing.T) {
	_, o := relay(t, shareedge.Config{Billing: quotaCatalog()})
	api := NewAPI(o.Server, o.Token, "test")
	acct, err := api.Whoami(context.Background())
	if err != nil || !acct.Billing || acct.Plan.ID != "free" || acct.Usage.Cap == nil || acct.Usage.Resets.IsZero() {
		t.Fatalf("account: %+v %v", acct, err)
	}
	link, err := api.BillingURL(context.Background())
	if err != nil || !strings.HasPrefix(link, "https://edge.share.test/account?t=") {
		t.Fatalf("billing URL: %s %v", link, err)
	}
}

// TestTrafficQuotaWaitingRecoversAcrossMonth preserves the public URL after a real metered transfer exhausts quota.
func TestTrafficQuotaWaitingRecoversAcrossMonth(t *testing.T) {
	var now atomic.Value
	now.Store(time.Date(2026, 1, 31, 12, 0, 0, 0, time.UTC))
	cfg := quotaCatalog()
	cfg.Plans[0].Traffic = 100
	e, o := relay(t, shareedge.Config{Billing: cfg, Now: func() time.Time { return now.Load().(time.Time) }})
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, strings.Repeat("x", 512)) }))
	defer local.Close()
	h, rec, link := open(t, o, local.Listener.Addr().String())
	response, err := browser(e).Get(link)
	if err == nil {
		io.Copy(io.Discard, response.Body)
		response.Body.Close()
	}
	rec.wait(t, EventWaiting, 2*time.Second)
	now.Store(time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC))
	h.Retry()
	live := rec.wait(t, EventLive, 2*time.Second)
	if live.URL != link || live.Remaining != 100 {
		t.Fatalf("month recovery: %+v", live)
	}
}
