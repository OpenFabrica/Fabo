package main

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/egoist/mygo/ui"
)

// shareAccount contains the billing fields of GET /fabo/share/account.
type shareAccount struct {
	Billing bool `json:"billing"`
	Plan    struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"plan"`
	Limits struct {
		Tunnels *uint64 `json:"tunnels"`
	} `json:"limits"`
	Usage struct {
		Used   uint64    `json:"used"`
		Cap    *uint64   `json:"cap"`
		Resets time.Time `json:"resets"`
	} `json:"usage"`
}

// exhausted reports whether the account has consumed this month's allowance.
func (a shareAccount) exhausted() bool {
	return a.Billing && a.Usage.Cap != nil && a.Usage.Used >= *a.Usage.Cap
}

// shareBillingToolbar renders the plan and its monthly transfer allowance.
func (s *desktop) shareBillingToolbar(c *ui.Context, p palette) {
	a := s.shares.billing
	ink, bg := p.sub, p.soft
	name := a.Plan.Name
	if a.Plan.ID == "free" {
		name = tr("免费版", "Free")
	} else {
		ink, bg = p.accent, p.accent.Alpha(.1)
	}
	ui.Text(c, name).FontSize(11).FontWeight(500).TextColor(ink).Padding(3, 8).Radius(4).Background(bg).Shrink(0)
	if a.Usage.Cap != nil {
		fraction := float64(a.Usage.Used) / float64(max(1, *a.Usage.Cap))
		ink = p.accent
		if fraction >= 1 {
			ink = p.err
		} else if fraction >= .8 {
			ink = p.warn
		}
		usage := displaySize(int64(a.Usage.Used)) + " / " + displaySize(int64(*a.Usage.Cap))
		if s.width < 1000 {
			ui.Column(c).Width(120).Gap(5).Shrink(0).Children(func() {
				label(c, usage, ink).FontSize(10)
				meter(c, p, fraction, 4, ink)
			})
		} else {
			ui.Row(c).Gap(10).AlignItems(ui.Center).Shrink(0).Children(func() {
				ui.Box(c).Width(120).Children(func() { meter(c, p, fraction, 4, ink) })
				label(c, usage, ink).FontSize(12)
			})
		}
	}
}

// shareQuotaBanner explains why links are waiting and offers an upgrade.
func (s *desktop) shareQuotaBanner(c *ui.Context, p palette) {
	a := s.shares.billing
	if !a.exhausted() {
		return
	}
	text := tr("本月流量已用完", "Monthly traffic exhausted")
	if !a.Usage.Resets.IsZero() {
		// The relay's timestamp defines the reset day; local time may be a different date.
		date := a.Usage.Resets
		text += fmt.Sprintf(tr("，%d 月 %d 日重置", "; resets %d/%d"), date.Month(), date.Day())
	}
	ui.Box(c).FillWidth().Padding(0, pageInset, 16).Children(func() {
		ui.Row(c.Key("share-quota")).FillWidth().Padding(12, 16).Gap(12).Radius(10).Background(p.err.Alpha(.08)).AlignItems(ui.Center).Children(func() {
			ui.Icon(c, icons["info"]).Size(16, 16).TextColor(p.err).Shrink(0)
			ui.Text(c, text).FontSize(12).TextColor(p.err).Grow(1).MinWidth(0)
			if labeledButton(c.Key("quota-upgrade"), p, tr("升级", "Upgrade"), tr("升级流量额度", "Upgrade traffic allowance"), "", dangerButton, s.working["share-billing"]) {
				s.openShareBilling()
			}
		})
	})
}

// shareLimitReached checks the live connection limit before opening New link.
func (s *desktop) shareLimitReached() bool {
	a := s.shares.billing
	if !s.shares.info.Account.SignedIn || !a.Billing || a.Limits.Tunnels == nil {
		return false
	}
	var live uint64
	for _, link := range s.shares.info.Shares {
		if link.State == "live" {
			live++
		}
	}
	return live >= *a.Limits.Tunnels
}

// shareLimitForm explains the current plan's connection limit.
func (s *desktop) shareLimitForm(c *ui.Context, p palette) {
	a := s.shares.billing
	name := a.Plan.Name
	if a.Plan.ID == "free" {
		name = tr("免费版", "Free")
	}
	var count uint64
	if a.Limits.Tunnels != nil {
		count = *a.Limits.Tunnels
	}
	ui.Text(c, fmt.Sprintf(tr("%s同时只能开 %d 个链接", "%s allows %d simultaneous links"), name, count)).FontSize(16).FontWeight(600).TextColor(p.text)
	ui.Text(c, tr("先停止一个链接，或升级套餐后再创建。", "Stop an existing link, or upgrade your plan to create more.")).FontSize(13).TextColor(p.sub)
}

// openShareBilling obtains a fresh single-use ticket without blocking the UI.
func (s *desktop) openShareBilling() {
	if s.working["share-billing"] {
		return
	}
	account := s.shares.info.Account
	s.working["share-billing"] = true
	s.run(15*time.Second, func(ctx context.Context) func() {
		var out struct {
			URL string `json:"url"`
		}
		err := s.engine.request(ctx, http.MethodPost, "/fabo/share/billing", nil, &out)
		if err == nil {
			u, parseErr := url.Parse(out.URL)
			if parseErr != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
				err = fmt.Errorf("invalid billing URL")
			}
		}
		return func() {
			s.working["share-billing"] = false
			current := s.shares.info.Account
			// A ticket belongs to the account that requested it, even if sign-out completes meanwhile.
			if !current.SignedIn || current.Login != account.Login || current.Server != account.Server {
				return
			}
			if err != nil {
				s.notices = append(s.notices, err.Error())
				return
			}
			s.shares.billingURL = out.URL
		}
	})
}
