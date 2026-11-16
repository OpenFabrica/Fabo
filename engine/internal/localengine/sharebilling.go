//go:build linux || darwin

package localengine

import (
	"context"
	"reflect"
	"time"

	"github.com/OpenFabrica/Fabo/engine/sharetunnel"
)

// cancelBillingLocked cancels account-specific polling and forgets the previous plan snapshot.
func (p *PublicShares) cancelBillingLocked() {
	if p.billingCancel != nil {
		p.billingCancel()
		p.billingCancel = nil
	}
	p.lastPlan = nil
}

// sameAccountLocked rejects responses from a relay or token that has since been replaced.
func (p *PublicShares) sameAccountLocked(api *sharetunnel.API) bool {
	return !p.closed && api.Token == p.cfg.EffectiveToken() && api.Server.String() == sharetunnel.NormalizeServer(p.cfg.EffectiveServer())
}

// observeAccount retries waiting links when a current account read shows available traffic or changed service.
func (p *PublicShares) observeAccount(api *sharetunnel.API, acct sharetunnel.Account) {
	p.mu.Lock()
	if !p.sameAccountLocked(api) {
		p.mu.Unlock()
		return
	}
	changed := p.lastPlan != nil && !reflect.DeepEqual(*p.lastPlan, acct.Plan)
	plan := acct.Plan
	p.lastPlan = &plan
	ready := acct.Billing && (changed || (acct.Usage.Cap != nil && acct.Usage.Used < *acct.Usage.Cap))
	var handles []*sharetunnel.Handle
	if ready {
		for _, s := range p.shares {
			if s.State == sharetunnel.EventWaiting && s.handle != nil {
				handles = append(handles, s.handle)
			}
		}
	}
	p.mu.Unlock()
	for _, h := range handles {
		h.Retry()
	}
}

// startBillingPoll replaces any previous payment watch with a bounded account-specific loop.
func (p *PublicShares) startBillingPoll(api *sharetunnel.API, interval, lifetime time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.sameAccountLocked(api) {
		return
	}
	if p.billingCancel != nil {
		p.billingCancel()
	}
	ctx, cancel := context.WithTimeout(context.Background(), lifetime)
	p.billingCancel = cancel
	p.billingWG.Add(1)
	go p.pollBilling(ctx, cancel, api, interval)
}

// pollBilling reads whoami without holding the engine lock and exits on logout, relay change or shutdown.
func (p *PublicShares) pollBilling(ctx context.Context, cancel context.CancelFunc, api *sharetunnel.API, interval time.Duration) {
	defer p.billingWG.Done()
	defer cancel()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		acct, err := api.Whoami(ctx)
		if err == nil {
			p.observeAccount(api, acct)
		}
	}
}
