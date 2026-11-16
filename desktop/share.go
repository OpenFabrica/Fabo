package main

// Share: public HTTPS links to a local port or container, through a
// fabo-edge relay. The engine hosts the tunnels (GET/POST/DELETE
// /fabo/share), so links opened here and by `fabo share` show together,
// and the account lives in the engine's share.json, not in desktop.json.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/ui"
)

const sharePage = "share"

// errSkipped marks a read the refresh didn't make.
var errSkipped = errors.New("not read")

// shareInfo is GET /fabo/share.
type shareInfo struct {
	Account struct {
		SignedIn bool   `json:"signedIn"`
		Login    string `json:"login"`
		Server   string `json:"server"`
		Pending  *struct {
			Code  string `json:"code"`
			URI   string `json:"uri"`
			Error string `json:"error"`
		} `json:"pending"`
	} `json:"account"`
	Shares []shareLink `json:"shares"`
}

// shareLink is one tunnel the engine hosts.
type shareLink struct {
	ID       string         `json:"id"`
	Target   string         `json:"target"`
	Name     string         `json:"name"`
	Owner    string         `json:"owner"`
	URL      string         `json:"url"`
	State    string         `json:"state"`
	Reason   string         `json:"reason"`
	Error    bool           `json:"error"`
	Started  time.Time      `json:"started"`
	Requests uint64         `json:"requests"`
	In       uint64         `json:"in"`
	Out      uint64         `json:"out"`
	Recent   []shareRequest `json:"recent"`
}

// shareRequest is one visitor request a link served.
type shareRequest struct {
	Method string    `json:"method"`
	Path   string    `json:"path"`
	Status int       `json:"status"`
	MS     int64     `json:"ms"`
	Out    uint64    `json:"out"`
	At     time.Time `json:"at"`
}

// shareUI is the page's own state.
type shareUI struct {
	info shareInfo
	// ok is set once the engine answered, so the page doesn't flash the
	// signed-out state while it loads.
	ok bool
	// waiting is set while the sign-in dialog waits for GitHub approval.
	waiting        bool
	billing        shareAccount
	billingChecked time.Time
	billingURL     string
	// watch holds the links this window opened and has not seen settle,
	// each with whether a list has shown it yet; ready holds the ones that
	// just went live, which the view announces.
	watch map[string]bool
	ready []shareLink
	// copy is an address the view writes to the clipboard on its next frame.
	copy string
}

// shareState is a link's state as a dot and a word.
func shareState(l shareLink) (kind, word string) {
	switch l.State {
	case "live":
		return "running", tr("在线", "Live")
	case "waiting":
		return "failed", tr("等待额度", "Waiting for quota")
	case "connecting", "reconnecting":
		return "busy", tr("连接中", "Connecting")
	}
	if l.Error {
		return "failed", tr("已断开", "Closed")
	}
	return "stopped", tr("已停止", "Stopped")
}

// shareView is the Share page: the account, the links and their requests.
func (s *desktop) shareView(c *ui.Context, p palette) {
	a := s.shares.info.Account
	toolbar(c, func() {
		switch {
		case a.SignedIn:
			dot(c, p, "running")
			ui.Text(c, a.Login).FontSize(12).FontWeight(500).TextColor(p.text)
			if s.shares.billing.Billing {
				s.shareBillingToolbar(c, p)
			} else {
				ui.Text(c, a.Server).FontSize(12).TextColor(p.muted).SingleLine().MinWidth(0)
			}
		case a.Server != "":
			ui.Text(c, a.Server).FontSize(12).TextColor(p.muted).SingleLine().MinWidth(0)
		}
		ui.Box(c).Grow(1)
		if !s.shares.ok {
			return
		}
		if a.SignedIn {
			if s.shares.billing.Billing {
				text := tr("升级", "Upgrade")
				if s.shares.billing.Plan.ID != "free" {
					text = tr("续费", "Renew")
				}
				if button(c.Key("share-upgrade"), p, text, "", plainButton, s.working["share-billing"]) {
					s.openShareBilling()
				}
			}
			if button(c.Key("new-share"), p, tr("新建链接", "New link"), "plus", primaryButton, s.working["share-create"]) {
				s.openShareForm("")
			}
		} else if a.Server != "" {
			if button(c.Key("share-login"), p, tr("登录", "Sign in"), "", primaryButton, false) {
				s.form = form{kind: "share-login"}
			}
		}
		entries := []menuEntry{{label: tr("更换中继…", "Change relay…"), run: func() { s.form = form{kind: "share-relay", host: a.Server} }}}
		if a.SignedIn {
			if s.shares.billing.Billing {
				entries = append(entries, menuEntry{label: tr("套餐与账单…", "Plans and billing…"), run: s.openShareBilling})
			}
			entries = append(entries, menuEntry{}, menuEntry{label: tr("退出登录", "Sign out"), danger: true, run: s.signOutShare})
		}
		s.moreMenu(c.Key("share-more"), p, tr("共享账号", "Share account"), entries)
	})
	if !s.shares.ok {
		return
	}
	switch {
	case a.Server == "":
		if empty(c, p, "globe", tr("设置共享中继", "Set a share relay"), tr("设置中继", "Set relay")) {
			s.form = form{kind: "share-relay"}
		}
		return
	case !a.SignedIn:
		if empty(c, p, "globe", tr("登录后把端口或容器发布到公网", "Sign in to publish a port or container"), tr("登录", "Sign in")) {
			s.form = form{kind: "share-login"}
		}
		return
	}
	s.shareQuotaBanner(c, p)
	links := s.shares.info.Shares
	cols := []column{{tr("目标", "Target"), 0, 1, false}, {tr("公开地址", "Public address"), 0, 1.6, false},
		{tr("状态", "State"), 110, 0, false}, {tr("请求", "Requests"), 70, 0, true}, {tr("流量", "Traffic"), 80, 0, true},
		{tr("开始", "Started"), 84, 0, true}}
	const actionsWidth = 62
	if !s.dockMax {
		tableHeader(c, p, narrowColumns(cols, s.narrow(), 3, 4, 5), actionsWidth, nil)
		if len(links) == 0 {
			if empty(c, p, "globe", tr("还没有公开链接", "No public links yet"), tr("新建链接", "New link")) {
				s.openShareForm("")
			}
		} else {
			narrow := s.narrow()
			ui.List(c.Key("rows"), s.list(sharePage), len(links)).FillWidth().Grow(1).MinHeight(0).
				ItemKey(func(i int) any { return links[i].ID }).
				Rows(func(r ui.ListRow) {
					rc, l := r.Context, links[r.Index]
					s.tableRow(rc, p, l.ID, l.Target, actionsWidth, nil, func() {
						cell(rc, 0, cols[0].grow).Gap(10).Children(func() { s.shareTarget(rc, p, l) })
						cell(rc, 0, cols[1].grow).Children(func() {
							switch {
							case l.URL == "":
								label(rc, "—", p.muted)
							case l.State != "live":
								// An address that does not answer now is not offered
								// as a link; one that will not come back by itself
								// is struck out.
								text := label(rc, shareAddress(l.URL), p.muted)
								if l.State == "waiting" || l.State == "ended" {
									text.Strikethrough()
								}
							default:
								// The address opens itself, as a container's does.
								if openLink(rc.Key("address"), p, "", shareAddress(l.URL), l.URL) {
									rc.OpenURL(l.URL)
								}
							}
						})
						state := cell(rc, cols[2].width, 0).Gap(7)
						if l.Reason != "" {
							state.Tooltip(l.Reason)
						}
						state.Children(func() {
							kind, word := shareState(l)
							dot(rc, p, kind)
							ink := p.text
							if kind == "failed" {
								ink = p.err
							}
							label(rc, word, ink)
						})
						if !narrow {
							cell(rc, cols[3].width, 0).Children(func() { number(rc, fmt.Sprint(l.Requests), p.text) })
							cell(rc, cols[4].width, 0).Children(func() { number(rc, displaySize(int64(l.In+l.Out)), p.text) })
							cell(rc, cols[5].width, 0).Children(func() { number(rc, since(s.now, l.Started.Unix()), p.muted) })
						}
					}, func() {
						s.shareActions(rc, p, l)
					})
				})
		}
	}
	for _, l := range links {
		if l.ID == s.selected[sharePage] {
			s.shareDock(c, p, l)
		}
	}
}

// shareAddress is a link's address as rows show it, without the scheme.
func shareAddress(address string) string {
	return strings.TrimPrefix(strings.TrimPrefix(address, "https://"), "http://")
}

// sharedContainer finds the container a link publishes. A port's target
// always carries a colon, which a container's name cannot.
func (s *desktop) sharedContainer(l shareLink) (container, bool) {
	if strings.Contains(l.Target, ":") {
		return container{}, false
	}
	for _, x := range s.containers {
		if x.Name == l.Target {
			return x, true
		}
	}
	return container{}, false
}

// shareOf finds the link that publishes a container, a live one first.
func (s *desktop) shareOf(x container) (found shareLink, ok bool) {
	for _, l := range s.shares.info.Shares {
		if l.Target != x.Name || l.State == "ended" {
			continue
		}
		if l.State == "live" {
			return l, true
		}
		if !ok {
			found, ok = l, true
		}
	}
	return found, ok
}

// shareTarget is a row's target: a box for a container, a screen for a port
// of this machine, then its name. A container that stopped says so, as its link answers
// nothing until it runs again.
func (s *desktop) shareTarget(c *ui.Context, p palette, l shareLink) {
	// A container that was removed is still a container's link.
	icon := "box"
	if strings.Contains(l.Target, ":") {
		icon = "monitor"
	}
	ui.Icon(c, icons[icon]).Size(14, 14).Shrink(0).TextColor(p.muted)
	label(c, l.Target, p.text).FontWeight(500)
	if x, ok := s.sharedContainer(l); ok && x.bucket() != "running" {
		label(c, tr("已停止", "Stopped"), p.warn).Shrink(0)
	}
}

// copyShare puts a link's address on the clipboard.
func (s *desktop) copyShare(c *ui.Context, l shareLink) {
	c.WriteClipboard(l.URL)
	s.notices = append(s.notices, tr("已复制 ", "Copied ")+shareAddress(l.URL))
}

// shareActions are a link row's buttons: copy and stop, or for a link the
// relay closed, try again and remove.
func (s *desktop) shareActions(c *ui.Context, p palette, l shareLink) {
	if l.State == "ended" {
		if iconButton(c.Key("retry"), p, "restart", tr("重新连接 ", "Reconnect ")+l.Target, s.working[l.ID]).Clicked() {
			s.startShare(l.Target, l.Name, l.ID)
		}
		if iconButton(c.Key("remove"), p, "x", tr("移除 ", "Remove ")+l.Target, s.working[l.ID]).Clicked() {
			s.stopShare(l)
		}
		return
	}
	if iconButton(c.Key("copy"), p, "copy", tr("复制地址 ", "Copy the address of ")+l.Target, l.URL == "").Clicked() {
		s.copyShare(c, l)
	}
	if iconButton(c.Key("stop"), p, "stop", tr("停止共享 ", "Stop sharing ")+l.Target, s.working[l.ID]).Clicked() {
		s.askStopShare(l)
	}
}

// shareDock shows a link: where it points and what it served lately.
func (s *desktop) shareDock(c *ui.Context, p palette, l shareLink) {
	kind, word := shareState(l)
	s.dock(c, p, l.Target, func() { label(c, word, stateColor(p, kind)).Shrink(0) }, []string{tr("信息", "Info"), tr("请求", "Requests")}, func() {
		if iconButton(c.Key("dock-copy"), p, "copy", tr("复制地址", "Copy address"), l.URL == "").Clicked() {
			s.copyShare(c, l)
		}
		if iconButton(c.Key("dock-open"), p, "out", tr("在浏览器打开", "Open in browser"), l.URL == "" || l.State != "live").Clicked() {
			c.OpenURL(l.URL)
		}
		if x, ok := s.sharedContainer(l); ok && iconButton(c.Key("dock-container"), p, "box", tr("查看容器 ", "Show container ")+x.Name, false).Clicked() {
			s.open(containersPage, x.ID)
		}
		if l.State == "ended" {
			if iconButton(c.Key("dock-retry"), p, "restart", tr("重新连接", "Reconnect"), s.working[l.ID]).Clicked() {
				s.startShare(l.Target, l.Name, l.ID)
			}
			if iconButton(c.Key("dock-remove"), p, "x", tr("移除链接", "Remove link"), s.working[l.ID]).Clicked() {
				s.stopShare(l)
			}
		} else if iconButton(c.Key("dock-stop"), p, "stop", tr("停止共享", "Stop sharing this link"), s.working[l.ID]).Clicked() {
			s.askStopShare(l)
		}
	}, func(tab int) {
		if tab == 0 {
			opened := tr("窗口", "This window")
			if l.Owner == "cli" {
				opened = tr("命令行", "Command line")
			}
			keyValues(c, p, compact([]field{
				{tr("公开地址", "Public address"), l.URL},
				{tr("目标", "Target"), l.Target},
				{tr("名称", "Name"), l.Name},
				{tr("状态", "State"), word},
				{tr("原因", "Reason"), l.Reason},
				{tr("来源", "Opened from"), opened},
				{tr("请求", "Requests"), fmt.Sprint(l.Requests)},
				{tr("流入", "In"), displaySize(int64(l.In))},
				{tr("流出", "Out"), displaySize(int64(l.Out))},
				{tr("开始", "Started"), strings.TrimPrefix(since(s.now, l.Started.Unix()), "—")},
			}))
			return
		}
		if len(l.Recent) == 0 {
			ui.Text(c, tr("还没有请求", "No requests yet")).FontSize(12).TextColor(p.muted).Padding(14, 16)
			return
		}
		// Newest first, padded like the info tab.
		ui.Scroll(c.Key("requests")).Fill().Grow(1).MinHeight(0).Children(func() {
			ui.Column(c).FillWidth().Padding(14, 16).Gap(8).Children(func() {
				for i := len(l.Recent) - 1; i >= 0; i-- {
					r := l.Recent[i]
					status, ink := "—", p.muted
					if r.Status != 0 {
						status, ink = fmt.Sprint(r.Status), p.text
					}
					if r.Status >= 500 || r.Status == 0 {
						ink = p.err
					}
					ui.Row(c.Key(fmt.Sprint("req-", i))).FillWidth().Gap(10).Children(func() {
						label(c, r.At.Local().Format("15:04:05"), p.muted).Shrink(0)
						label(c, r.Method, p.text).Width(52).Shrink(0)
						label(c, status, ink).Width(32).Shrink(0)
						label(c, r.Path, p.text).Grow(1)
						label(c, fmt.Sprintf("%d ms", r.MS), p.muted).Shrink(0)
					})
				}
			})
		})
	})
}

// openShareForm opens New link. A container, when given, is already chosen,
// so typing starts at the name; without running containers it opens on the
// port. Opening it reads the account again, so the plan's limit is known by
// the time the form is sent.
func (s *desktop) openShareForm(container string) {
	if s.shareLimitReached() {
		s.form = form{kind: "share-limit"}
		return
	}
	s.form = form{kind: "share", host: container, named: container != "", flag: container == "" && len(s.runningNames()) == 0}
	s.refreshing = false
	s.refresh()
}

// runningNames are the containers a link can publish.
func (s *desktop) runningNames() []string {
	var names []string
	for _, x := range s.containers {
		if x.bucket() == "running" {
			names = append(names, x.Name)
		}
	}
	return names
}

// shareForm is the New link dialog's fields: what to publish, a container
// (form.host) or a port of this machine (form.image, with form.flag set),
// and the name its address should carry.
func (s *desktop) shareForm(c *ui.Context, p palette) (submitted bool) {
	f := &s.form
	mode := 0
	if f.flag {
		mode = 1
	}
	// The row keeps the control at its own width; the dialog would stretch it.
	ui.Row(c).FillWidth().Children(func() {
		if choice(c.Key("share-kind"), p, tr("共享什么", "What to share"), &mode, tr("容器", "Container"), tr("本机端口", "Local port")) {
			f.flag, f.problem = mode == 1, ""
		}
	})
	if f.flag {
		submitted = input(c, p, "target", tr("端口或 host:port", "Port or host:port"), "3000", &f.image, true).Submitted()
	} else {
		// Nothing is chosen until the user chooses: a link is public at once.
		none := tr("选择容器", "Pick a container")
		options := append([]string{none}, s.runningNames()...)
		chosen := none
		if f.host != "" {
			chosen = f.host
			// The chosen container may have stopped since the dialog opened.
			if !slices.Contains(options, chosen) {
				options = append(options, chosen)
			}
		}
		ui.Column(c.Key("field-container")).FillWidth().Gap(5).Children(func() {
			ui.Text(c, tr("容器", "Container")).FontSize(11.5).TextColor(p.muted)
			if len(options) == 1 {
				ui.Text(c, tr("没有运行中的容器", "No running containers")).FontSize(12).TextColor(p.sub).Padding(7, 0)
				return
			}
			if ui.Select(c.Key("share-container"), &chosen, options).Label(tr("要共享的容器", "Container to share")).FillWidth().Height(30).
				Cursor(ui.CursorPointer).Changed() {
				f.host, f.problem = "", ""
				if chosen != none {
					f.host = chosen
				}
			}
		})
	}
	// The name is where typing starts for a container: Enter then shares it.
	return input(c, p, "name", tr("名称", "Name"), tr("自动生成", "Generated"), &f.name, !f.flag).Submitted() || submitted
}

// createShare checks New link and asks the engine to open it.
func (s *desktop) createShare() bool {
	f := &s.form
	target := f.host
	if f.flag {
		target = f.image
	}
	if target == "" {
		f.problem = tr("请选择要共享的容器", "Pick a container to share")
		if f.flag {
			f.problem = tr("请填写端口", "Enter a port")
		}
		return false
	}
	// The account read that opening the form asked for may have brought a
	// limit the window did not know of.
	if s.shareLimitReached() {
		s.form = form{kind: "share-limit"}
		return false
	}
	s.startShare(target, f.name, "")
	return true
}

// startShare opens a link to target and watches it until it settles;
// replaces, when set, is a closed link it takes the place of. Nothing is
// announced here: the link is only useful once it is live, and the view
// says so then.
func (s *desktop) startShare(target, name, replaces string) {
	key := "share-create"
	if replaces != "" {
		key = replaces
	}
	s.working[key] = true
	s.run(2*time.Minute, func(ctx context.Context) func() {
		if replaces != "" {
			// Gone already is as good as removed.
			_ = s.engine.request(ctx, http.MethodDelete, "/fabo/share/"+url.PathEscape(replaces), nil, nil)
		}
		var out struct {
			ID string `json:"id"`
		}
		err := s.engine.request(ctx, http.MethodPost, "/fabo/share", map[string]any{"target": target, "name": name}, &out)
		return func() {
			delete(s.working, key)
			if err == nil && out.ID != "" {
				if s.shares.watch == nil {
					s.shares.watch = map[string]bool{}
				}
				s.shares.watch[out.ID] = false
				if s.selected[sharePage] == replaces && replaces != "" {
					s.selected[sharePage] = out.ID
				}
			}
			s.settle(err, "")
		}
	})
	if s.engine.http == nil {
		delete(s.working, key)
	}
}

// noteShares looks at a fresh list for the links this window opened: one
// that went live is handed to the view to announce, one that cannot serve
// is reported with the relay's reason. Either way it is watched no longer.
func (s *desktop) noteShares(links []shareLink) {
	if len(s.shares.watch) == 0 {
		return
	}
	listed := map[string]bool{}
	for _, l := range links {
		listed[l.ID] = true
		if _, watched := s.shares.watch[l.ID]; !watched {
			continue
		}
		s.shares.watch[l.ID] = true
		switch l.State {
		case "live":
			s.shares.ready = append(s.shares.ready, l)
		case "waiting", "ended":
			_, word := shareState(l)
			text := l.Target + tr("：", ": ") + word
			if l.Reason != "" {
				text += " · " + l.Reason
			}
			s.notices = append(s.notices, text)
		default:
			continue
		}
		delete(s.shares.watch, l.ID)
	}
	// A link stopped before it settled has nothing left to say. One no
	// list has shown yet stays: a read that began before the link was
	// opened ends after it, without the link.
	for id, seen := range s.shares.watch {
		if seen && !listed[id] {
			delete(s.shares.watch, id)
		}
	}
}

// announceShares turns what noteShares found into toasts, and performs the
// copy a toast's button asked for. Only the view can do either.
func (s *desktop) announceShares(c *ui.Context) {
	if s.shares.copy != "" {
		c.WriteClipboard(s.shares.copy)
		s.notices = append(s.notices, tr("已复制 ", "Copied ")+shareAddress(s.shares.copy))
		s.shares.copy = ""
	}
	for _, l := range s.shares.ready {
		c.ToastAction(fmt.Sprintf(tr("%s 已上线", "%s is live"), l.Target), tr("复制链接", "Copy link"), func() { s.shares.copy = l.URL })
	}
	s.shares.ready = nil
}

// askStopShare stops a link, asking first when it has an address: one the
// relay generated cannot be had again.
func (s *desktop) askStopShare(l shareLink) {
	if l.URL == "" {
		s.stopShare(l)
		return
	}
	s.pending = &confirmation{
		title: tr("停止共享 ", "Stop sharing ") + l.Target + tr("？", "?"),
		body:  fmt.Sprintf(tr("%s 会立即无法访问。", "%s stops answering at once."), shareAddress(l.URL)),
		ok:    tr("停止共享", "Stop sharing"),
		run:   func() { s.stopShare(l) },
	}
}

// stopShare closes a link. It can be asked from another page, so the Share
// page's selection is dropped here and not through selectRow.
func (s *desktop) stopShare(l shareLink) {
	s.do(l.ID, engineAction{http.MethodDelete, "/fabo/share/" + url.PathEscape(l.ID), nil}, tr("已停止共享 ", "Stopped sharing ")+l.Target)
	if s.selected[sharePage] == l.ID {
		s.selected[sharePage] = ""
		if s.page == sharePage {
			s.dockMax = false
		}
	}
}

// shareContainer is the containers page's quick share: New link with the
// container chosen, or whatever stands before that, a relay or a sign-in.
func (s *desktop) shareContainer(x container) {
	a := s.shares.info.Account
	switch {
	case a.Server == "":
		s.form = form{kind: "share-relay"}
	case !a.SignedIn:
		s.form = form{kind: "share-login"}
	default:
		s.openShareForm(x.Name)
	}
}

// shareControl is a running container's share button, in its row and in its
// panel (id keeps their menus apart). Not shared, it opens New link for the
// container. Shared, it takes the link's state color and opens the link's
// menu: copy, open, details, stop.
func (s *desktop) shareControl(c *ui.Context, p palette, id string, x container) {
	if !s.shares.ok || x.bucket() != "running" {
		return
	}
	l, shared := s.shareOf(x)
	if !shared || !s.shares.info.Account.SignedIn {
		// The globe is in the accent even at rest, so the way to the
		// public internet stands out among the row's grey buttons.
		name := tr("共享 ", "Share ") + x.Name
		busy := s.working["share-create"]
		b := ui.ButtonBase(c.Key("share")).Label(name).Tooltip(name).Size(28, 28).Shrink(0).Center().Radius(7).Disabled(busy).Cursor(pointerFor(busy))
		ink := p.accent
		switch {
		case busy:
			ink = p.muted.Alpha(0.4)
		case b.Hovered():
			b.Background(p.accent.Alpha(0.14))
		}
		b.Children(func() { ui.Icon(c, icons["globe"]).Size(15, 15).TextColor(ink) })
		if b.Clicked() {
			s.shareContainer(x)
		}
		return
	}
	kind, word := shareState(l)
	tint := stateColor(p, kind)
	first := menuEntry{label: word + "…", disabled: true}
	if l.URL != "" {
		first = menuEntry{label: fmt.Sprintf(tr("复制 %s", "Copy %s"), shareAddress(l.URL)), run: func() { s.copyShare(c, l) }}
	}
	if kind == "running" {
		// Live is the accent here: green is the container's own state.
		tint = p.accent
	}
	s.menuButton(c.Key("share"), p, id, "globe", tint, tr("公开链接 ", "Public link of ")+x.Name, []menuEntry{
		first,
		{label: tr("在浏览器打开", "Open in browser"), disabled: l.State != "live", run: func() { c.OpenURL(l.URL) }},
		{label: tr("共享详情", "Share details"), run: func() { s.open(sharePage, l.ID) }},
		{},
		{label: tr("停止共享", "Stop sharing"), danger: true, run: func() { s.askStopShare(l) }},
	})
}

// signOutShare signs out; the engine closes the links first.
func (s *desktop) signOutShare() {
	s.do("share-logout", engineAction{http.MethodPost, "/fabo/share/logout", map[string]any{}}, tr("已退出共享账号", "Signed out of sharing"))
}

// shareRelayForm is the Change relay dialog's field.
func (s *desktop) shareRelayForm(c *ui.Context, p palette) bool {
	return input(c, p, "relay", tr("中继地址", "Relay"), "edge.example.com", &s.form.host, true).Submitted()
}

// setShareRelay points the account at another relay; a different one
// signs out and closes the links that used the old one.
func (s *desktop) setShareRelay() bool {
	server := strings.TrimSpace(s.form.host)
	if server == "" {
		s.form.problem = tr("请填写中继地址", "Enter the relay's address")
		return false
	}
	s.do("share-relay", engineAction{http.MethodPost, "/fabo/share/login", map[string]any{"server": server}}, "")
	return true
}

// shareLoginForm is the Sign in dialog: GitHub, or a token from the relay's
// operator; while GitHub waits, the code to type there.
func (s *desktop) shareLoginForm(c *ui.Context, p palette) (submitted bool) {
	f := &s.form
	pending := s.shares.info.Account.Pending
	if s.shares.waiting && pending != nil {
		ui.Text(c, tr("在 GitHub 输入此代码", "Enter this code on GitHub")).FontSize(12).TextColor(p.sub)
		ui.Text(c, pending.Code).Font(mono).FontSize(22).FontWeight(600).LetterSpacing(2).TextColor(p.text).Selectable()
		ui.Row(c).FillWidth().Gap(8).Children(func() {
			if button(c.Key("open-github"), p, tr("打开 GitHub", "Open GitHub"), "out", plainButton, false) {
				c.OpenURL(pending.URI)
			}
			if button(c.Key("copy-code"), p, tr("复制代码", "Copy code"), "copy", plainButton, false) {
				c.WriteClipboard(pending.Code)
			}
		})
		if pending.Error != "" {
			f.problem = pending.Error
		} else {
			ui.Text(c, tr("等待确认…", "Waiting for approval…")).FontSize(12).TextColor(p.muted)
		}
		return false
	}
	if button(c.Key("github-login"), p, tr("用 GitHub 登录", "Sign in with GitHub"), "", plainButton, s.working["share-github"]) {
		s.startGitHubLogin()
	}
	ui.Divider(c)
	return input(c, p, "token", tr("Token", "Token"), "ft_…", &f.value, true).Submitted()
}

// startGitHubLogin asks the engine to begin the device flow, then opens
// the code page; the engine polls GitHub and the page's refresh notices.
func (s *desktop) startGitHubLogin() {
	s.working["share-github"] = true
	s.run(30*time.Second, func(ctx context.Context) func() {
		var info struct {
			Pending *struct {
				Code, URI string
			} `json:"pending"`
		}
		var raw []byte
		err := s.engine.request(ctx, http.MethodPost, "/fabo/share/login", map[string]any{"github": true}, &raw)
		if err == nil && len(raw) > 0 {
			err = json.Unmarshal(raw, &info)
		}
		return func() {
			delete(s.working, "share-github")
			if err != nil {
				s.form.problem = err.Error()
				return
			}
			s.form.problem = ""
			s.shares.waiting = true
			if info.Pending != nil {
				s.shares.info.Account.Pending = &struct {
					Code  string `json:"code"`
					URI   string `json:"uri"`
					Error string `json:"error"`
				}{Code: info.Pending.Code, URI: info.Pending.URI}
				if s.window != nil && strings.HasPrefix(info.Pending.URI, "https://") {
					go mygo.Shell.OpenExternal(info.Pending.URI)
				}
			}
		}
	})
	if s.engine.http == nil {
		delete(s.working, "share-github")
	}
}

// loginShare submits the Sign in dialog: a token signs in at once; while
// GitHub waits it just closes.
func (s *desktop) loginShare() bool {
	f := &s.form
	if s.shares.waiting {
		return true
	}
	token := strings.TrimSpace(f.value)
	if token == "" {
		f.problem = tr("填写 token，或用 GitHub 登录", "Enter a token, or sign in with GitHub")
		return false
	}
	s.do("share-login", engineAction{http.MethodPost, "/fabo/share/login", map[string]any{"token": token}}, tr("已登录", "Signed in"))
	return true
}

// settleShareLogin closes the Sign in dialog once GitHub approved.
func (s *desktop) settleShareLogin() {
	a := s.shares.info.Account
	if s.form.kind == "share-login" && s.shares.waiting && a.SignedIn && a.Pending == nil {
		s.form = form{}
		s.shares.waiting = false
		s.notices = append(s.notices, tr("已登录 ", "Signed in as ")+a.Login)
	}
	if s.form.kind != "share-login" {
		s.shares.waiting = false
	}
}
