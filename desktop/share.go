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
	waiting bool
}

// shareState is a link's state as a dot and a word.
func shareState(l shareLink) (kind, word string) {
	switch l.State {
	case "live":
		return "running", tr("在线", "Live")
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
			ui.Text(c, a.Server).FontSize(12).TextColor(p.muted).SingleLine().MinWidth(0)
		case a.Server != "":
			ui.Text(c, a.Server).FontSize(12).TextColor(p.muted).SingleLine().MinWidth(0)
		}
		ui.Box(c).Grow(1)
		if !s.shares.ok {
			return
		}
		if a.SignedIn {
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
			entries = append(entries, menuEntry{}, menuEntry{label: tr("退出登录", "Sign out"), danger: true, run: s.signOutShare})
		}
		s.moreMenu(c.Key("share-more"), p, tr("共享账号", "Share account"), entries)
	})
	if !s.shares.ok {
		return
	}
	switch {
	case a.Server == "":
		if empty(c, p, "share", tr("设置共享中继", "Set a share relay"), tr("设置中继", "Set relay")) {
			s.form = form{kind: "share-relay"}
		}
		return
	case !a.SignedIn:
		if empty(c, p, "share", tr("登录后把端口或容器发布到公网", "Sign in to publish a port or container"), tr("登录", "Sign in")) {
			s.form = form{kind: "share-login"}
		}
		return
	}
	links := s.shares.info.Shares
	cols := []column{{tr("目标", "Target"), 0, 1, false}, {tr("地址", "Address"), 0, 1.4, false},
		{tr("状态", "State"), 110, 0, false}, {tr("请求", "Requests"), 80, 0, true}}
	const actionsWidth = 92
	if !s.dockMax {
		tableHeader(c, p, narrowColumns(cols, s.narrow(), 3), actionsWidth, nil)
		if len(links) == 0 {
			if empty(c, p, "share", tr("还没有公开链接", "No public links yet"), tr("新建链接", "New link")) {
				s.openShareForm("")
			}
		} else {
			narrow := s.narrow()
			ui.List(c.Key("rows"), s.list(sharePage), len(links)).FillWidth().Grow(1).MinHeight(0).
				ItemKey(func(i int) any { return links[i].ID }).
				Rows(func(r ui.ListRow) {
					rc, l := r.Context, links[r.Index]
					s.tableRow(rc, p, l.ID, l.Target, actionsWidth, nil, func() {
						cell(rc, 0, cols[0].grow).Gap(10).Children(func() {
							ui.Icon(rc, icons["share"]).Size(14, 14).TextColor(p.muted)
							label(rc, l.Target, p.text).FontWeight(500)
						})
						cell(rc, 0, cols[1].grow).Children(func() {
							address := l.URL
							if address == "" {
								address = "—"
							}
							label(rc, address, p.sub)
						})
						cell(rc, cols[2].width, 0).Gap(7).Children(func() {
							kind, word := shareState(l)
							dot(rc, p, kind)
							label(rc, word, p.text)
						})
						if !narrow {
							cell(rc, cols[3].width, 0).Children(func() { number(rc, fmt.Sprint(l.Requests), p.text) })
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

// shareActions are a link row's buttons: copy, open, stop.
func (s *desktop) shareActions(c *ui.Context, p palette, l shareLink) {
	if iconButton(c.Key("copy"), p, "copy", tr("复制地址 ", "Copy the address of ")+l.Target, l.URL == "").Clicked() {
		c.WriteClipboard(l.URL)
		s.notices = append(s.notices, tr("已复制", "Copied"))
	}
	if iconButton(c.Key("open"), p, "out", tr("在浏览器打开 ", "Open in the browser ")+l.Target, l.URL == "").Clicked() {
		c.OpenURL(l.URL)
	}
	if iconButton(c.Key("stop"), p, "stop", tr("停止共享 ", "Stop sharing ")+l.Target, s.working[l.ID]).Clicked() {
		s.stopShare(l)
	}
}

// shareDock shows a link: where it points and what it served lately.
func (s *desktop) shareDock(c *ui.Context, p palette, l shareLink) {
	_, word := shareState(l)
	s.dock(c, p, l.Target, func() { label(c, word, p.muted).Shrink(0) }, []string{tr("信息", "Info"), tr("请求", "Requests")}, func() {
		if iconButton(c.Key("dock-stop"), p, "stop", tr("停止共享 ", "Stop sharing ")+l.Target, s.working[l.ID]).Clicked() {
			s.stopShare(l)
		}
	}, func(tab int) {
		if tab == 0 {
			opened := tr("窗口", "This window")
			if l.Owner == "cli" {
				opened = tr("命令行", "Command line")
			}
			keyValues(c, p, compact([]field{
				{tr("地址", "Address"), l.URL},
				{tr("目标", "Target"), l.Target},
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

// openShareForm opens New link, with target filled in when given.
func (s *desktop) openShareForm(target string) {
	s.form = form{kind: "share", image: target, named: target != ""}
}

// shareForm is the New link dialog's fields.
func (s *desktop) shareForm(c *ui.Context, p palette) (submitted bool) {
	f := &s.form
	var running []string
	for _, x := range s.containers {
		if x.bucket() == "running" {
			running = append(running, x.Name)
		}
	}
	ui.Row(c).FillWidth().Gap(10).AlignItems(ui.End).Children(func() {
		submitted = input(c, p, "target", tr("端口或容器", "Port or container"), "3000", &f.image, !f.named).Grow(1).Submitted()
		if len(running) > 0 {
			none := tr("选择容器", "Pick a container")
			if chosen, ok := pick(c.Key("share-container"), tr("容器", "Container"), none, append([]string{none}, running...)); ok && chosen != none {
				f.image = chosen
			}
		}
	})
	submitted = input(c, p, "name", tr("名称", "Name"), tr("自动生成", "Generated"), &f.name, f.named).Submitted() || submitted
	return submitted
}

// createShare checks New link and asks the engine to open it.
func (s *desktop) createShare() bool {
	f := &s.form
	if f.image == "" {
		f.problem = tr("请填写端口或容器", "Enter a port or container")
		return false
	}
	target, name := f.image, f.name
	s.do("share-create", engineAction{http.MethodPost, "/fabo/share", map[string]any{"target": target, "name": name}},
		tr("已打开公开链接 ", "Opened a public link to ")+target)
	return true
}

// stopShare closes a link.
func (s *desktop) stopShare(l shareLink) {
	s.do(l.ID, engineAction{http.MethodDelete, "/fabo/share/" + url.PathEscape(l.ID), nil}, tr("已停止共享 ", "Stopped sharing ")+l.Target)
	if s.selected[sharePage] == l.ID {
		s.selectRow("")
	}
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
