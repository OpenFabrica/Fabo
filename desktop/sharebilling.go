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
		ID      string     `json:"id"`
		Name    string     `json:"name"`
		Expires *time.Time `json:"expires"`
	} `json:"plan"`
	Limits struct {
		Tunnels *uint64 `json:"tunnels"`
	} `json:"limits"`
	Usage struct {
		Used    uint64    `json:"used"`
		Cap     *uint64   `json:"cap"`
		Resets  time.Time `json:"resets"`
		History []struct {
			Month string `json:"month"`
			Used  uint64 `json:"used"`
		} `json:"history"`
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
		ink = shareUsageInk(p, fraction)
		usage := displaySize(int64(a.Usage.Used)) + " / " + displaySize(int64(*a.Usage.Cap))
		// The whole meter opens the traffic details, so it reads as one control.
		b := clickable(c.Key("share-usage")).Label(tr("本月流量 ", "Monthly traffic ")+usage).Tooltip(tr("查看流量", "View traffic")).
			Padding(4, 6).Radius(6).Shrink(0).AlignItems(ui.Center)
		if b.Hovered() {
			b.Background(p.raised)
		}
		if b.Clicked() {
			s.form = form{kind: "share-usage"}
		}
		b.Children(func() {
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
		})
	}
}

// shareUsageInk colors traffic by how much of the allowance is gone.
func shareUsageInk(p palette, fraction float64) ui.Color {
	switch {
	case fraction >= 1:
		return p.err
	case fraction >= .8:
		return p.warn
	}
	return p.accent
}

// shareUsageForm is the traffic dialog: this month against the allowance,
// when it resets, the plan's expiry, recent months and the open links.
func (s *desktop) shareUsageForm(c *ui.Context, p palette) {
	a := s.shares.billing
	ui.Text(c, tr("流量", "Traffic")).FontSize(16).FontWeight(600).TextColor(p.text)
	if a.Usage.Cap != nil {
		capacity := *a.Usage.Cap
		fraction := float64(a.Usage.Used) / float64(max(1, capacity))
		ink := shareUsageInk(p, fraction)
		ui.Column(c).FillWidth().Gap(8).Children(func() {
			ui.Row(c).FillWidth().AlignItems(ui.End).Gap(6).Children(func() {
				ui.Text(c, displaySize(int64(a.Usage.Used))).FontSize(22).FontWeight(600).FontFeatures("tnum").TextColor(ink)
				ui.Text(c, "/ "+displaySize(int64(capacity))).FontSize(13).TextColor(p.sub).Padding(0, 0, 3)
				ui.Box(c).Grow(1)
				ui.Text(c, fmt.Sprintf("%.0f%%", min(1, fraction)*100)).FontSize(12).FontFeatures("tnum").TextColor(p.sub).Padding(0, 0, 3)
			})
			meter(c, p, fraction, 6, ink)
		})
		remaining := uint64(0)
		if a.Usage.Used < capacity {
			remaining = capacity - a.Usage.Used
		}
		name := a.Plan.Name
		if a.Plan.ID == "free" {
			name = tr("免费版", "Free")
		}
		fields := []field{
			{tr("剩余", "Remaining"), displaySize(int64(remaining))},
			{tr("套餐", "Plan"), name},
		}
		if !a.Usage.Resets.IsZero() {
			fields = append(fields, field{tr("重置", "Resets"), a.Usage.Resets.Format("2006-01-02")})
		}
		if a.Plan.Expires != nil {
			fields = append(fields, field{tr("到期", "Expires"), a.Plan.Expires.Local().Format("2006-01-02")})
		}
		keyValues(c, p, fields)
	}
	if len(a.Usage.History) > 1 {
		shareHistory(c, p, a)
	}
	var in, out uint64
	live := 0
	for _, l := range s.shares.info.Shares {
		in, out, live = in+l.In, out+l.Out, live+1
	}
	if live > 0 {
		ui.Text(c, fmt.Sprintf(tr("当前 %d 个链接：流入 %s，流出 %s", "%d open links: %s in, %s out"), live, displaySize(int64(in)), displaySize(int64(out)))).
			FontSize(12).TextColor(p.sub)
	}
}

// shareHistory draws recent months as bars scaled to their own peak, the
// current month last and in the accent color.
func shareHistory(c *ui.Context, p palette, a shareAccount) {
	history := a.Usage.History
	ui.Column(c).FillWidth().Gap(6).Children(func() {
		ui.Text(c, tr("近几个月", "Recent months")).FontSize(12).TextColor(p.sub)
		ui.Box(c.Key("share-history")).FillWidth().Height(64).Draw(func(pt *ui.Painter, r ui.Rect) {
			var peak uint64 = 1
			for _, m := range history {
				peak = max(peak, m.Used)
			}
			slot := r.W / float32(len(history))
			for i, m := range history {
				h := max(2, float32(m.Used)/float32(peak)*r.H)
				ink := p.accent.Alpha(.35)
				if i == len(history)-1 {
					ink = p.accent
				}
				pt.Fill(ui.Rect{X: r.X + float32(i)*slot + slot*.2, Y: r.Y + r.H - h, W: slot * .6, H: h}, ink, 3)
			}
		})
		ui.Row(c).FillWidth().Children(func() {
			for _, m := range history {
				month := m.Month
				if t, err := time.Parse("2006-01", m.Month); err == nil {
					month = tr(fmt.Sprintf("%d 月", t.Month()), t.Month().String()[:3])
				}
				ui.Column(c).Width(0).Grow(1).AlignItems(ui.Center).Children(func() {
					ui.Text(c, displaySize(int64(m.Used))).Font(mono).FontSize(10).TextColor(p.sub).SingleLine()
					ui.Text(c, month).FontSize(10.5).TextColor(p.muted).SingleLine()
				})
			}
		})
	})
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
	// The page opens in the window's language; read it here, not in the request goroutine.
	language := currentLang()
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
			} else {
				q := u.Query()
				q.Set("lang", language)
				u.RawQuery = q.Encode()
				out.URL = u.String()
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
