package main

// The Debug page: socket sessions in the manner of a socket debugger. TCP,
// UDP and WebSocket clients and servers, an HTTP client and an HTTP mock,
// each a saved session (debug_session.go) with its live sockets
// (debug_net.go). Local sessions need no engine; a client may target a
// container's port, through its published port or the engine's relay.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/egoist/mygo/ui"
)

const debugPage = "debug"

// debugInput is what is being typed into one session's composer.
type debugInput struct {
	text    string
	hex     bool
	repeat  bool
	every   string
	keep    bool
	problem string
	// stop ends the repeat loop of the session, if one runs.
	stop context.CancelFunc
}

// debugDraft is the new-session (or edit) dialog's fields.
type debugDraft struct {
	editing   string
	kind      string
	proto     string
	byAddress bool
	target    string
	via       string
	address   string
	path      string
	port      string
	name      string
	bindAll   bool
	greeting  string
	tls       bool
	cert, key string
	auto      bool
	note      string
	insecure  bool
	// other is a port typed by hand on the chosen container, for one the
	// lists do not show (UDP, or one not listening yet).
	other string
}

// debugRouteDraft is the route dialog's fields; index is the route edited,
// or -1 for a new one.
type debugRouteDraft struct {
	index                                  int
	method, path, status, ctype, body, dir string
	delay                                  string
}

// debugUI is the page's own state.
type debugUI struct {
	loaded   bool
	path     string
	sessions []debugSession
	runs     map[string]*debugRun
	cur      string
	inputs   map[string]*debugInput
	showHex  map[string]bool
	filter   string
	draft    debugDraft
	route    debugRouteDraft
	reqTab   int
	resTab   int
	viewing  map[string]int
	split    float32
	pending  atomic.Bool
	copy     string
	// relayNames are the containers behind connections the engine relayed
	// to this computer, by the address a server here saw; relayTries
	// counts the lookups that found nothing, as the engine records a relay
	// a moment after the server accepts it.
	relayNames  map[string]string
	relayTries  map[string]int
	relayAsking bool
	// listening holds the TCP ports each running container listens on, by
	// name, read when the new-session dialog opens: unpublished ports are
	// not in the container list.
	listening map[string][]int
	// autoStarted marks that the servers listening with the window were
	// started.
	autoStarted bool
	// back is the page the debug page was opened from, where the debug
	// menu's back button returns.
	back string
}

// debugState loads the saved sessions once and starts the servers that
// listen with the window.
func (s *desktop) debugState() *debugUI {
	d := &s.debug
	// The settings file is known once the app is ready; sessions read
	// before that (none) are read again from it, while nothing runs.
	if d.loaded && (d.path == debugSessionsPath(s.prefsFile) || len(d.runs) > 0) {
		return d
	}
	d.loaded, d.autoStarted = true, false
	d.runs, d.inputs, d.showHex, d.viewing = map[string]*debugRun{}, map[string]*debugInput{}, map[string]bool{}, map[string]int{}
	d.relayNames, d.relayTries = map[string]string{}, map[string]int{}
	d.path = debugSessionsPath(s.prefsFile)
	if d.path != "" {
		debugCertDir = filepath.Dir(d.path)
	}
	d.sessions = loadDebugSessions(d.path)
	if len(d.sessions) > 0 {
		d.cur = d.sessions[0].ID
	}
	return d
}

// debugAutoListen starts, once, the servers that listen when Fabo opens.
// On Linux it waits for the networks (whose gateways the servers also
// listen on) unless forced, which the window does a few seconds after it
// opens in case the engine is not answering.
func (s *desktop) debugAutoListen(force bool) {
	d := s.debugState()
	if d.autoStarted || !force && runtime.GOOS == "linux" && len(s.networks) == 0 {
		return
	}
	d.autoStarted = true
	for _, x := range d.sessions {
		if x.Kind == debugServer && x.AutoListen {
			s.startDebug(x)
		}
	}
}

// debugRedraw asks the window to redraw for a run's news, at most once per
// frame however many lines arrive.
func (s *desktop) debugRedraw() {
	if s.window == nil || s.debug.pending.Swap(true) {
		return
	}
	s.window.Update(func() { s.debug.pending.Store(false) })
}

// debugRun is a session's run, made on first use.
func (s *desktop) debugRunOf(id string) *debugRun {
	d := s.debugState()
	r := d.runs[id]
	if r == nil {
		r = newDebugRun(s.debugRedraw)
		d.runs[id] = r
	}
	return r
}

// debugSession finds a session by ID.
func (s *desktop) debugSession(id string) (debugSession, int) {
	for i, x := range s.debug.sessions {
		if x.ID == id {
			return x, i
		}
	}
	return debugSession{}, -1
}

// saveDebug writes the sessions; a failure is told, not fatal.
func (s *desktop) saveDebug() {
	if err := saveDebugSessions(s.debug.path, s.debug.sessions); err != nil {
		s.notices = append(s.notices, tr("调试会话未能保存：", "Debug sessions were not saved: ")+err.Error())
	}
}

// debugLive counts the sessions holding sockets.
func (s *desktop) debugLive() int {
	n := 0
	for _, r := range s.debug.runs {
		if r.active() {
			n++
		}
	}
	return n
}

// startDebug opens a session's sockets, resolving a container target first.
func (s *desktop) startDebug(x debugSession) {
	r := s.debugRunOf(x.ID)
	if x.Kind == debugServer {
		x.Binds = s.debugBinds()
		r.start(s.ctx, x, nil)
		return
	}
	dial, err := s.debugDial(x)
	if err != nil {
		r.fail(err)
		return
	}
	if x.Proto == debugHTTP {
		go r.doHTTP(s.ctx, x, dial)
		return
	}
	r.start(s.ctx, x, dial)
}

// debugBinds are the addresses a loopback server also listens on so that
// containers reach it. On Linux the engine runs beside the window and
// containers reach host.docker.internal at their bridge's gateway; on a Mac
// the engine relays it to the loopback, which is enough.
func (s *desktop) debugBinds() []string {
	if runtime.GOOS != "linux" {
		return nil
	}
	var out []string
	for _, n := range s.networks {
		if ip := net.ParseIP(n.Gateway); n.Driver == "bridge" && ip != nil && ip.To4() != nil && !slices.Contains(out, n.Gateway) {
			out = append(out, n.Gateway)
		}
	}
	return out
}

// debugDial picks how a client reaches its target: directly, or for a
// container through its published port on the loopback or through the
// engine's relay into the container network.
func (s *desktop) debugDial(x debugSession) (dialFunc, error) {
	if x.Container == "" {
		return directDial, nil
	}
	var found *container
	for i := range s.containers {
		if s.containers[i].Name == x.Container || s.containers[i].ID == x.Container {
			found = &s.containers[i]
		}
	}
	if found == nil {
		return nil, fmt.Errorf(tr("容器 %s 不存在", "Container %s does not exist"), x.Container)
	}
	if found.State != "running" {
		return nil, fmt.Errorf(tr("容器 %s 已停止", "Container %s is stopped"), x.Container)
	}
	network := x.Proto
	if network != debugUDP {
		network = debugTCP
	}
	if x.Via == "net" {
		name, port, engine := found.Name, x.Port, s.engine
		return func(ctx context.Context, _, _ string) (net.Conn, error) {
			return engine.dialContainer(ctx, name, port, network)
		}, nil
	}
	for _, pm := range found.Ports {
		if pm.Private == x.Port && pm.Public > 0 && (pm.Type == "" || pm.Type == network) {
			address := fmt.Sprintf("127.0.0.1:%d", pm.Public)
			return func(ctx context.Context, _, _ string) (net.Conn, error) { return directDial(ctx, network, address) }, nil
		}
	}
	return nil, fmt.Errorf(tr("%s 的 %d/%s 没有映射到本机", "Port %[2]d/%[3]s of %[1]s is not published"), x.Container, x.Port, network)
}

// debugTargetText is a session's target as its row shows it.
func debugTargetText(x debugSession) string {
	if x.Kind == debugServer {
		return fmt.Sprintf(":%d", x.Port)
	}
	return clientTarget(x)
}

// protoInk is the color of a protocol's badge.
func protoInk(p palette, proto string) ui.Color {
	switch proto {
	case debugWS:
		return p.accent
	case debugHTTP:
		return p.ok
	case debugUDP:
		return p.warn
	}
	return p.sub
}

// debugStateWords is a run's state as a word and its color.
func (s *desktop) debugStateWords(p palette, x debugSession, snap debugSnap) (string, ui.Color) {
	since := ""
	if !snap.since.IsZero() {
		since = " " + ago(int64(time.Since(snap.since).Seconds()))
	}
	switch snap.state {
	case debugBusy:
		if x.Proto == debugHTTP && x.Kind == debugClient {
			return tr("请求中…", "Requesting…"), p.warn
		}
		return tr("连接中…", "Connecting…"), p.warn
	case debugOpen:
		return tr("已连接", "Connected") + since, p.ok
	case debugListening:
		return tr("监听中", "Listening") + since, p.ok
	case debugFailed:
		return snap.problem, p.err
	}
	if x.Kind == debugServer {
		return tr("未在监听", "Not listening"), p.muted
	}
	if x.Proto == debugHTTP {
		return "", p.muted
	}
	return tr("未连接", "Not connected"), p.muted
}

// debugView is the page: the chosen session's workspace. The sessions
// themselves are listed in the sidebar's debug menu.
func (s *desktop) debugView(c *ui.Context, p palette) {
	d := s.debugState()
	if d.copy != "" {
		c.WriteClipboard(d.copy)
		s.notices = append(s.notices, tr("已复制 ", "Copied ")+d.copy)
		d.copy = ""
	}
	if len(d.sessions) == 0 {
		if empty(c, p, "pulse", tr("还没有调试会话", "No debug sessions yet"), tr("新建会话", "New session")) {
			s.openDebugForm(debugDraft{})
		}
		return
	}
	if _, i := s.debugSession(d.cur); i < 0 {
		d.cur = d.sessions[0].ID
	}
	x, _ := s.debugSession(d.cur)
	ui.Column(c.Key("debug-work-" + x.ID)).FillWidth().Grow(1).MinHeight(0).Children(func() {
		s.debugWorkspace(c, p, x)
	})
}

// debugMenu replaces the sidebar on the debug page: a way back to the page
// it came from, the new-session button, then the clients and the servers.
// Folded, the sessions show as their protocol badges.
func (s *desktop) debugMenu(c *ui.Context, p palette, open bool) {
	d := s.debugState()
	back := func() {
		page := d.back
		if page == "" || page == debugPage {
			page = overviewPage
		}
		s.navigate(page)
	}
	if !open {
		if iconButton(c.Key("debug-back"), p, "back", tr("返回", "Back"), false).Clicked() {
			back()
		}
		if iconButton(c.Key("debug-new"), p, "plus", tr("新建会话", "New session"), false).Clicked() {
			s.openDebugForm(debugDraft{})
		}
		ui.Box(c).Width(24).Height(1).Shrink(0).Margin(8, 0).Background(p.line)
		ui.Scroll(c.Key("debug-sessions")).FillWidth().Grow(1).MinHeight(0).Children(func() {
			ui.Column(c).FillWidth().Gap(4).AlignItems(ui.Center).Children(func() {
				for _, kind := range []string{debugClient, debugServer} {
					for _, x := range s.debugShown(kind) {
						s.debugRow(c.Key("row-"+x.ID), p, x, false)
					}
				}
			})
		})
		return
	}
	ui.Row(c.Key("debug-menu-head")).FillWidth().Height(36).Shrink(0).Gap(6).AlignItems(ui.Center).Children(func() {
		if iconButton(c.Key("debug-back"), p, "back", tr("返回", "Back"), false).Clicked() {
			back()
		}
		ui.Text(c, pageTitle(debugPage)).FontSize(15).FontWeight(600).TextColor(p.text).SingleLine().Grow(1).MinWidth(0)
		if iconButton(c.Key("debug-new"), p, "plus", tr("新建会话", "New session"), false).Clicked() {
			s.openDebugForm(debugDraft{})
		}
	})
	ui.Scroll(c.Key("debug-sessions")).FillWidth().Grow(1).MinHeight(0).Children(func() {
		ui.Column(c).FillWidth().Gap(2).Children(func() {
			for _, g := range []struct{ kind, title string }{{debugClient, tr("客户端", "Clients")}, {debugServer, tr("服务端", "Servers")}} {
				shown := s.debugShown(g.kind)
				if len(shown) == 0 {
					continue
				}
				ui.Text(c, g.title).FontSize(11).FontWeight(600).LetterSpacing(0.4).TextColor(p.muted).Padding(14, 10, 4)
				for _, x := range shown {
					s.debugRow(c.Key("row-"+x.ID), p, x, true)
				}
			}
		})
	})
}

// debugShown lists the clients or the servers, filtered by the search.
func (s *desktop) debugShown(kind string) []debugSession {
	query := strings.ToLower(strings.TrimSpace(s.search))
	var shown []debugSession
	for _, x := range s.debug.sessions {
		if x.Kind == kind && (query == "" || strings.Contains(strings.ToLower(x.Name+" "+debugTargetText(x)+" "+x.Note), query)) {
			shown = append(shown, x)
		}
	}
	return shown
}

// debugRow is one session in the debug menu: its protocol badge with the
// state, then, open, its name and target. The chosen one is lifted off the
// shell the way the sidebar marks its current page.
func (s *desktop) debugRow(c *ui.Context, p palette, x debugSession, open bool) {
	snap := s.debugRunOf(x.ID).snapshot()
	on := s.debug.cur == x.ID
	b := clickable(c).Label(x.Name).Radius(9).Shrink(0).AlignItems(ui.Center)
	if open {
		b.FillWidth().Padding(7, 8).Gap(10).Justify(ui.Start)
	} else {
		b.Size(40, 40).Justify(ui.Center).Tooltip(x.Name + " · " + debugTargetText(x))
	}
	switch {
	case on:
		b.Background(p.panel).Border(1, p.line).Shadow(0, 1, 2, 0, ui.RGBA(16, 24, 40, 0.05))
	case b.Hovered():
		b.Background(p.raised)
	}
	b.Children(func() {
		ink := protoInk(p, x.Proto)
		ui.Box(c).Size(34, 30).Shrink(0).Radius(7).Center().Background(ink.Alpha(0.13)).Children(func() {
			ui.Text(c, x.badge()).Font(mono).FontSize(10).FontWeight(600).TextColor(ink)
			if x.Proto != debugHTTP || x.Kind == debugServer {
				state := "stopped"
				switch snap.state {
				case debugOpen, debugListening:
					state = "running"
				case debugFailed:
					state = "failed"
				case debugBusy:
					state = "busy"
				}
				ui.Box(c).Absolute().Right(-3).Bottom(-3).Size(12, 12).Radius(6).Background(p.panel).Center().Children(func() {
					dot(c, p, state)
				})
			}
		})
		if !open {
			return
		}
		ui.Column(c).Width(0).Grow(1).MinWidth(0).Gap(2).Children(func() {
			ui.Text(c, x.Name).FontSize(13).FontWeight(500).TextColor(p.text).SingleLine()
			sub, ink := debugTargetText(x), p.link
			if x.Kind == debugServer {
				ink = p.muted
				if snap.state == debugListening && x.Proto == debugHTTP {
					sub = fmt.Sprintf(tr("监听 %s", "Listening %s"), sub)
				} else if snap.state == debugListening {
					live := 0
					for _, peer := range snap.peers {
						if !peer.Gone {
							live++
						}
					}
					if x.Proto == debugUDP {
						sub = fmt.Sprintf(tr("监听 %s · %d 个来源", "Listening %s · %d senders"), sub, live)
					} else {
						sub = fmt.Sprintf(tr("监听 %s · %d 个连接", "Listening %s · %d connected"), sub, live)
					}
				}
			}
			ui.Text(c, sub).Font(mono).FontSize(11).TextColor(ink).SingleLine()
		})
	})
	if b.Clicked() {
		s.debug.cur = x.ID
	}
}

// debugWorkspace is the chosen session: its head and its kind's body.
func (s *desktop) debugWorkspace(c *ui.Context, p palette, x debugSession) {
	r := s.debugRunOf(x.ID)
	snap := r.snapshot()
	s.debugHead(c, p, x, r, snap)
	switch {
	case x.Kind == debugClient && x.Proto == debugHTTP:
		s.debugHTTPClient(c, p, x, r, snap)
	case x.Kind == debugServer && x.Proto == debugHTTP:
		s.debugMock(c, p, x, r, snap)
	case x.Kind == debugServer:
		s.debugServerBody(c, p, x, r, snap)
	default:
		s.debugClientBody(c, p, x, r, snap)
	}
}

// debugHead is a session's name, state, target and actions.
func (s *desktop) debugHead(c *ui.Context, p palette, x debugSession, r *debugRun, snap debugSnap) {
	ui.Row(c.Key("debug-head")).FillWidth().Shrink(0).Padding(14, 20, 10).Gap(10).AlignItems(ui.Center).Children(func() {
		ui.Text(c, x.Name).FontSize(16).FontWeight(700).TextColor(p.text).SingleLine().Shrink(0).MaxWidth(240)
		words, ink := s.debugStateWords(p, x, snap)
		if words != "" {
			ui.Text(c, words).FontSize(12).FontWeight(600).TextColor(ink).SingleLine().MinWidth(0).Shrink(1)
		}
		if x.Kind == debugClient && x.Proto != debugHTTP {
			chip(c, p, "link", clientTarget(x), p.link)
		}
		if x.Kind == debugServer {
			chip(c, p, "", protoName(x.Proto)+" "+x.listenAddress(), p.sub)
		}
		if x.Container != "" {
			b := clickable(c.Key("debug-container")).Label(tr("打开容器 ", "Open container ") + x.Container).Height(24).PaddingX(8).Gap(5).Radius(6).
				AlignItems(ui.Center).Background(p.soft).Shrink(0)
			b.Children(func() {
				ui.Icon(c, icons["box"]).Size(12, 12).TextColor(p.sub)
				ui.Text(c, x.Container).FontSize(12).TextColor(p.sub)
			})
			if b.Clicked() {
				s.openContainerByName(x.Container)
			}
		}
		ui.Box(c).Grow(1)
		if x.Proto != debugHTTP || x.Kind == debugServer {
			busy := snap.state == debugBusy
			switch {
			case r.active() && x.Kind == debugServer:
				if button(c.Key("debug-stop"), p, tr("停止", "Stop"), "", removeButton, false) {
					go r.stop()
				}
			case r.active():
				if button(c.Key("debug-stop"), p, tr("断开", "Disconnect"), "", removeButton, busy) {
					go r.stop()
				}
			case x.Kind == debugServer:
				if button(c.Key("debug-start"), p, tr("开始监听", "Listen"), "", primaryButton, false) {
					s.startDebug(x)
				}
			default:
				if button(c.Key("debug-start"), p, tr("连接", "Connect"), "", primaryButton, false) {
					s.startDebug(x)
				}
			}
		}
		entries := []menuEntry{
			{label: tr("编辑…", "Edit…"), run: func() { s.openDebugForm(draftOf(x)) }},
			{label: tr("复制为新会话", "Duplicate"), run: func() { s.duplicateDebug(x) }},
		}
		if x.Kind == debugServer {
			label := tr("启动 Fabo 时自动监听", "Listen when Fabo opens")
			if x.AutoListen {
				label = "✓ " + label
			}
			entries = append(entries, menuEntry{label: label, run: func() { s.updateDebug(x.ID, func(y *debugSession) { y.AutoListen = !y.AutoListen }) }})
		}
		if x.Proto != debugHTTP {
			entries = append(entries, menuEntry{label: tr("导出日志…", "Export log…"), run: func() { s.exportDebugLog(x, r) }})
		}
		entries = append(entries, menuEntry{}, menuEntry{label: tr("删除会话", "Delete session"), danger: true, run: func() { s.deleteDebug(x) }})
		s.moreMenu(c.Key("debug-more"), p, tr("会话操作", "Session actions"), entries)
	})
	if x.Note != "" {
		ui.Text(c, x.Note).FontSize(12).TextColor(p.muted).Padding(0, 20, 8).Shrink(0)
	}
}

// chip is a small tinted label with an optional icon.
func chip(c *ui.Context, p palette, icon, text string, ink ui.Color) {
	ui.Row(c).Height(24).PaddingX(8).Gap(5).Radius(6).AlignItems(ui.Center).Background(p.soft).MinWidth(0).Shrink(1).Children(func() {
		if icon != "" {
			ui.Icon(c, icons[icon]).Size(12, 12).Shrink(0).TextColor(ink)
		}
		ui.Text(c, text).Font(mono).FontSize(12).TextColor(ink).SingleLine()
	})
}

// openContainerByName shows a container on the containers page.
func (s *desktop) openContainerByName(name string) {
	for _, x := range s.containers {
		if x.Name == name {
			s.open(containersPage, x.ID)
			return
		}
	}
	s.notices = append(s.notices, fmt.Sprintf(tr("容器 %s 不存在", "Container %s does not exist"), name))
}

// debugBar is the line above a log: text or hex, the encoding, a search,
// and clearing.
func (s *desktop) debugBar(c *ui.Context, p palette, x debugSession, r *debugRun) {
	d := &s.debug
	ui.Row(c.Key("debug-bar")).FillWidth().Shrink(0).Padding(0, 20, 10).Gap(8).AlignItems(ui.Center).Children(func() {
		view := 0
		if d.showHex[x.ID] {
			view = 1
		}
		if filters(c.Key("debug-view"), p, tr("显示方式", "Show as"), &view, []string{tr("文本", "Text"), tr("16 进制", "Hex")}, nil) {
			d.showHex[x.ID] = view == 1
		}
		enc := x.Encoding
		if enc == "" {
			enc = "UTF-8"
		}
		if ui.Select(c.Key("debug-enc"), &enc, debugEncodings).Label(tr("编码", "Encoding")).Height(32).MinWidth(108).Shrink(0).Cursor(ui.CursorPointer).Changed() {
			s.updateDebug(x.ID, func(y *debugSession) { y.Encoding = enc })
		}
		ui.Box(c).Grow(1)
		ui.SearchField(c.Key("debug-find"), &d.filter).Placeholder(tr("在消息中查找", "Find in messages")).Label(tr("在消息中查找", "Find in messages")).
			Width(200).Shrink(1).MinWidth(100).Height(32).Radius(8)
		if iconButton(c.Key("debug-clear"), p, "trash", tr("清空消息", "Clear messages"), false).Clicked() {
			r.clear()
		}
	})
}

// debugLog shows a run's lines on the console ground, following the end.
func (s *desktop) debugLog(c *ui.Context, x debugSession, snap debugSnap, names func(string) string, emptyText string) {
	d := &s.debug
	query := strings.ToLower(strings.TrimSpace(d.filter))
	hex := d.showHex[x.ID]
	var shown []debugLine
	for _, l := range snap.lines {
		if query == "" || strings.Contains(strings.ToLower(l.Note+" "+string(l.Data)+" "+names(l.Peer)), query) {
			shown = append(shown, l)
		}
	}
	state := s.list("debug-log-" + x.ID)
	state.FollowEnd = true
	if len(shown) == 0 {
		ui.Box(c.Key("debug-log-empty")).Grow(1).MinHeight(0).Margin(0, 20, 0).Radius(10).Background(consoleTheme.Background).Padding(14).Children(func() {
			ui.Text(c, emptyText).Font(mono).FontSize(12).TextColor(logDimInk)
		})
		return
	}
	size := s.prefs.LogSize + 1
	copyPalette := palette{muted: logDimInk, text: logInk, raised: logInk.Alpha(0.12)}
	copyLabel := tr("复制数据包（16 进制）", "Copy packet as hex")
	ui.List(c.Key("debug-log"), state, len(shown)).Grow(1).MinHeight(0).Margin(0, 20, 0).Padding(8, 0).Radius(10).
		Background(consoleTheme.Background).Rows(func(row ui.ListRow) {
		rc, l := row.Context, shown[row.Index]
		ui.Row(rc).FillWidth().PaddingX(14).Gap(8).AlignItems(ui.Start).Children(func() {
			ui.Text(rc, l.At.Format("15:04:05.000")).Font(mono).FontSize(size).LineHeight(1.6).TextColor(logGutter).Shrink(0)
			arrow, ink := "•", logDimInk
			switch l.Dir {
			case lineSent:
				arrow, ink = "→", ui.Hex("#86A6FF")
			case lineReceived:
				arrow, ink = "←", ui.Hex("#4CC38A")
			case lineError:
				arrow, ink = "!", logErrInk
			}
			ui.Text(rc, arrow).Font(mono).FontSize(size).FontWeight(700).LineHeight(1.6).TextColor(ink).Width(12).Shrink(0)
			ui.Row(rc).Width(0).Grow(1).MinWidth(0).Gap(8).AlignItems(ui.Start).Children(func() {
				if l.Peer != "" {
					ui.Text(rc, names(l.Peer)).Font(mono).FontSize(size).LineHeight(1.6).TextColor(logWarnInk).Shrink(0)
				}
				switch {
				case l.Dir == lineNote || l.Dir == lineError:
					text := logDimInk
					if l.Dir == lineError {
						text = logErrInk
					}
					ui.Text(rc, l.Note).Font(mono).FontSize(size).LineHeight(1.6).TextColor(text).Width(0).Grow(1).MinWidth(0).Selectable()
				case hex:
					pairs, ascii := hexDump(l.Data)
					ui.Text(rc, pairs).Font(mono).FontSize(size).LineHeight(1.6).TextColor(logInk).Width(0).Grow(1).MinWidth(0).Selectable()
					ui.Text(rc, ascii).Font(mono).FontSize(size).LineHeight(1.6).TextColor(logDimInk).Shrink(0).MaxWidth(160)
				default:
					ui.Text(rc, decodeText(l.Data, x.Encoding)).Font(mono).FontSize(size).LineHeight(1.6).TextColor(logInk).Width(0).Grow(1).MinWidth(0).Selectable()
				}
				if l.Auto {
					ui.Text(rc, tr("自动", "auto")).FontSize(10.5).TextColor(logDimInk).Padding(1, 5).Radius(4).Background(ui.Hex("#FFFFFF").Alpha(0.08)).Shrink(0)
				}
			})
			if len(l.Data) > 0 {
				ui.Textf(rc, "%d B", len(l.Data)).Font(mono).FontSize(size - 1).LineHeight(1.75).TextColor(logGutter).Shrink(0)
			}
			if l.Dir == lineSent || l.Dir == lineReceived {
				if iconButton(rc.Key("copy-packet"), copyPalette, "copy", copyLabel, false).Size(22, 20).Clicked() {
					pairs, _ := hexDump(l.Data)
					rc.WriteClipboard(pairs)
					s.notices = append(s.notices, tr("已复制", "Copied"))
				}
			}
		})
	})
}

// debugClientBody is a TCP, UDP or WebSocket client: the log and the
// composer.
func (s *desktop) debugClientBody(c *ui.Context, p palette, x debugSession, r *debugRun, snap debugSnap) {
	s.debugBar(c, p, x, r)
	emptyText := tr("还没有消息", "No messages yet")
	if !r.active() {
		emptyText = tr("未连接", "Not connected")
	}
	s.debugLog(c, x, snap, func(a string) string { return a }, emptyText)
	s.debugComposer(c, p, x, !r.active() || snap.state != debugOpen, "", func(data []byte, binary bool) error {
		return r.sendClient(data, binary)
	})
}

// peerNamer names a server's peers by container where it can: a peer
// whose address is a container's is shown as the container.
func (s *desktop) peerNamer() func(string) string {
	byIP := map[string]string{}
	for _, x := range s.containers {
		for _, n := range x.Networks {
			if n.IP != "" {
				byIP[n.IP] = x.Name
			}
		}
	}
	relayed := s.debug.relayNames
	return func(addr string) string {
		if name := relayed[addr]; name != "" {
			return name
		}
		host, _, err := net.SplitHostPort(addr)
		if err != nil {
			return addr
		}
		if name := byIP[host]; name != "" {
			return name
		}
		if host == "127.0.0.1" || host == "::1" {
			return tr("本机", "This Mac")
		}
		return addr
	}
}

// askRelayNames looks up, once per peer and with a few retries, which
// containers are behind the loopback peers of a server: those the engine
// relayed from host.docker.internal.
func (s *desktop) askRelayNames(c *ui.Context, x debugSession, snap debugSnap) {
	d := &s.debug
	var ask []string
	waiting := false
	consider := func(addr string) {
		if !loopbackPeer(addr) || d.relayNames[addr] != "" || d.relayTries[addr] >= 3 || slices.Contains(ask, addr) {
			return
		}
		if d.relayTries[addr] > 0 {
			waiting = true
		}
		ask = append(ask, addr)
	}
	for _, peer := range snap.peers {
		if !peer.Gone {
			consider(peer.Addr)
		}
	}
	for _, h := range snap.hits {
		consider(h.Peer)
	}
	if len(ask) == 0 || d.relayAsking {
		return
	}
	if waiting {
		// Retry after a moment rather than on every frame.
		c.After(150 * time.Millisecond)
	}
	proto := debugTCP
	if x.Proto == debugUDP {
		proto = debugUDP
	}
	d.relayAsking = true
	s.run(5*time.Second, func(ctx context.Context) func() {
		found, err := s.engine.debugPeers(ctx, proto, ask)
		return func() {
			d.relayAsking = false
			for _, addr := range ask {
				if info, ok := found[addr]; ok && info.Container != "" {
					d.relayNames[addr] = info.Container
				} else if err == nil {
					d.relayTries[addr]++
				} else {
					d.relayTries[addr] = 3
				}
			}
		}
	})
	if s.engine.http == nil {
		d.relayAsking = false
	}
}

// debugAddresses shows where a server is reached: from this computer or
// the network, and from containers.
func (s *desktop) debugAddresses(c *ui.Context, p palette, x debugSession) {
	scheme := ""
	switch x.Proto {
	case debugWS:
		scheme = "ws://"
		if x.TLS {
			scheme = "wss://"
		}
	case debugHTTP:
		scheme = "http://"
		if x.TLS {
			scheme = "https://"
		}
	case debugUDP:
		scheme = "udp://"
	}
	local, localTitle := fmt.Sprintf("%s127.0.0.1:%d", scheme, x.Port), tr("本机", "This Mac")
	if x.BindAll {
		if ip := lanAddress(); ip != "" {
			local, localTitle = fmt.Sprintf("%s%s:%d", scheme, ip, x.Port), tr("局域网", "Network")
		}
	}
	inside := fmt.Sprintf("%shost.docker.internal:%d", scheme, x.Port)
	ui.Row(c.Key("debug-addresses")).FillWidth().Shrink(0).Padding(0, 20, 10).Gap(8).Children(func() {
		for i, a := range [][2]string{{localTitle, local}, {tr("容器内", "In containers"), inside}} {
			ui.Row(c.Key(i)).Width(0).Grow(1).MinWidth(0).Height(36).Padding(0, 6, 0, 12).Gap(10).Radius(8).Background(p.soft).AlignItems(ui.Center).Children(func() {
				ui.Text(c, a[0]).FontSize(12).TextColor(p.muted).Shrink(0)
				ui.Text(c, a[1]).Font(mono).FontSize(12.5).TextColor(p.text).SingleLine().Width(0).Grow(1).MinWidth(0).Selectable()
				if iconButton(c.Key("copy"), p, "copy", tr("复制 ", "Copy ")+a[1], false).Clicked() {
					s.debug.copy = a[1]
				}
				if x.Proto == debugHTTP && i == 0 {
					if iconButton(c.Key("open"), p, "out", tr("在浏览器中打开", "Open in browser"), false).Clicked() {
						c.OpenURL(a[1])
					}
				}
			})
		}
	})
}

// lanAddress is this computer's first private IPv4 address.
func lanAddress() string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return ""
	}
	for _, a := range addrs {
		if ip, ok := a.(*net.IPNet); ok && ip.IP.To4() != nil && ip.IP.IsPrivate() && !strings.HasPrefix(ip.IP.String(), "192.168.64.") {
			return ip.IP.String()
		}
	}
	return ""
}

// debugServerBody is a TCP, UDP or WebSocket server: its addresses, its
// peers beside the log, and the composer that writes to the picked peers.
func (s *desktop) debugServerBody(c *ui.Context, p palette, x debugSession, r *debugRun, snap debugSnap) {
	s.askRelayNames(c, x, snap)
	names := s.peerNamer()
	s.debugAddresses(c, p, x)
	s.debugBar(c, p, x, r)
	var live, picked []debugPeer
	for _, peer := range snap.peers {
		if !peer.Gone {
			live = append(live, peer)
			if peer.Picked {
				picked = append(picked, peer)
			}
		}
	}
	ui.Row(c.Key("debug-server")).FillWidth().Grow(1).MinHeight(0).AlignItems(ui.Stretch).Children(func() {
		ui.Column(c.Key("debug-peers")).Width(220).Shrink(0).Margin(0, 0, 0, 20).Radius(10).Border(1, p.line).Clip().Children(func() {
			ui.Row(c).FillWidth().Height(36).Padding(0, 6, 0, 12).Gap(6).AlignItems(ui.Center).BorderWidth(0, 0, 1, 0).BorderColor(p.line).Children(func() {
				title := tr("连接", "Connections")
				if x.Proto == debugUDP {
					title = tr("来源", "Senders")
				}
				ui.Textf(c, "%s %d", title, len(live)).FontSize(12).FontWeight(600).TextColor(p.sub).Grow(1)
				if iconButton(c.Key("debug-drop-gone"), p, "trash", tr("清空已断开", "Remove closed"), len(live) == len(snap.peers)).Clicked() {
					r.dropGone()
				}
			})
			ui.Scroll(c.Key("debug-peer-list")).FillWidth().Grow(1).MinHeight(0).Children(func() {
				if len(snap.peers) == 0 {
					text := tr("等待连接", "Waiting for connections")
					if !r.active() {
						text = tr("未在监听", "Not listening")
					}
					ui.Text(c, text).FontSize(12.5).TextColor(p.muted).Padding(14)
				}
				for _, peer := range snap.peers {
					s.debugPeerRow(c.Key("peer-"+peer.Addr), p, r, peer, names)
				}
			})
			ui.Row(c).FillWidth().Height(40).PaddingX(12).Gap(10).AlignItems(ui.Center).BorderWidth(1, 0, 0, 0).BorderColor(p.line).Children(func() {
				on := x.AutoReply
				if ui.Switch(c.Key("debug-auto"), &on).Label(tr("自动回复", "Auto reply")).Changed() {
					s.updateDebug(x.ID, func(y *debugSession) { y.AutoReply = on })
				}
				ui.Text(c, tr("自动回复", "Auto reply")).FontSize(12.5).TextColor(p.sub).Shrink(0)
				if x.AutoReply {
					reply := x.Reply
					field := ui.TextInput(c.Key("debug-reply"), &reply).Label(tr("自动回复内容", "Auto reply text")).
						Placeholder(tr("留空则原样回显", "Empty echoes")).Width(0).Grow(1).MinWidth(0).Height(26).Font(mono).FontSize(11.5)
					if field.Changed() {
						s.updateDebug(x.ID, func(y *debugSession) { y.Reply = reply })
					}
				}
			})
		})
		ui.Column(c.Key("debug-server-log")).Width(0).Grow(1).MinWidth(0).Margin(0, 0, 0, -10).Children(func() {
			emptyText := tr("等待消息", "Waiting for messages")
			if !r.active() {
				emptyText = tr("未在监听", "Not listening")
			}
			s.debugLog(c, x, snap, names, emptyText)
		})
	})
	target := tr("未选连接", "No connection picked")
	switch {
	case len(picked) == 1:
		target = tr("发给 ", "To ") + names(picked[0].Addr)
	case len(picked) > 1:
		target = fmt.Sprintf(tr("群发 %d 个连接", "To %d connections"), len(picked))
	}
	s.debugComposer(c, p, x, !r.active() || len(picked) == 0, target, func(data []byte, binary bool) error {
		_, err := r.sendPeers(data, binary)
		return err
	})
}

// debugPeerRow is one peer, ticked when it is sent to.
func (s *desktop) debugPeerRow(c *ui.Context, p palette, r *debugRun, peer debugPeer, names func(string) string) {
	b := clickable(c).Label(names(peer.Addr)+" "+peer.Addr).FillWidth().Padding(8, 12).Gap(9).AlignItems(ui.Center).Justify(ui.Start).Disabled(peer.Gone)
	if b.Hovered() && !peer.Gone {
		b.Background(p.hover)
	}
	if peer.Gone {
		b.Opacity(0.5)
	}
	b.Children(func() {
		box := ui.Box(c).Size(16, 16).Shrink(0).Radius(4).Center()
		if peer.Picked && !peer.Gone {
			box.Background(p.accent).Children(func() { ui.Icon(c, icons["check"]).Size(12, 12).TextColor(p.onAccent) })
		} else {
			box.Background(p.panel).Border(1.5, p.muted.Alpha(0.5))
		}
		ui.Column(c).Width(0).Grow(1).MinWidth(0).Children(func() {
			ui.Text(c, names(peer.Addr)).FontSize(12.5).FontWeight(600).TextColor(p.text).SingleLine()
			ui.Text(c, peer.Addr).Font(mono).FontSize(11).TextColor(p.muted).SingleLine()
		})
		since := ago(int64(time.Since(peer.Since).Seconds()))
		if peer.Gone {
			since = tr("已断开", "Closed")
		}
		ui.Text(c, since).FontSize(11).TextColor(p.muted).Shrink(0)
	})
	if b.Clicked() && !peer.Gone {
		r.pick(peer.Addr, !peer.Picked)
	}
}

// debugInputOf is a session's composer state.
func (s *desktop) debugInputOf(id string) *debugInput {
	in := s.debug.inputs[id]
	if in == nil {
		in = &debugInput{every: "1", keep: false}
		s.debug.inputs[id] = in
	}
	return in
}

// debugComposer is where messages are typed: text or hex, the quick
// inputs, repeating, and send. disabled holds it while nothing can be sent;
// target says where a server's message goes.
func (s *desktop) debugComposer(c *ui.Context, p palette, x debugSession, disabled bool, target string, send func([]byte, bool) error) {
	in := s.debugInputOf(x.ID)
	if disabled && in.stop != nil {
		in.stop()
		in.stop, in.repeat = nil, false
	}
	encode := func(text string) ([]byte, error) {
		if in.hex {
			return parseHex(text)
		}
		return encodeText(text, x.Encoding, x.NoEscape)
	}
	doSend := func(text string) {
		data, err := encode(text)
		if err == nil {
			err = send(data, in.hex)
		}
		if err != nil {
			in.problem = err.Error()
			return
		}
		in.problem = ""
		if !in.keep && text == in.text && !in.repeat {
			in.text = ""
		}
	}
	ui.Column(c.Key("debug-composer")).Shrink(0).Margin(10, 20, 16).Radius(10).Border(1, p.line).Background(p.panel).Children(func() {
		ui.Row(c).FillWidth().Padding(8, 10, 0).Gap(8).AlignItems(ui.Center).Clip().Children(func() {
			mode := 0
			if in.hex {
				mode = 1
			}
			if filters(c.Key("debug-mode"), p, tr("输入方式", "Input as"), &mode, []string{tr("文本", "Text"), tr("16 进制", "Hex")}, nil) {
				in.hex = mode == 1
			}
			for i, q := range x.Quick {
				b := clickable(c.Key(i)).Label(tr("快捷输入 ", "Quick input ") + q).Tooltip(q).Height(26).PaddingX(8).Radius(6).Background(p.soft).
					AlignItems(ui.Center).MaxWidth(150).Shrink(0).Disabled(disabled)
				if b.Hovered() {
					b.Background(p.raised)
				}
				b.Children(func() { ui.Text(c, q).Font(mono).FontSize(11.5).TextColor(p.sub).SingleLine() })
				if b.Clicked() && !disabled {
					doSend(q)
				}
				if b.RightClicked() {
					s.updateDebug(x.ID, func(y *debugSession) { y.Quick = slices.Delete(slices.Clone(y.Quick), i, i+1) })
				}
			}
			add := clickable(c.Key("debug-quick-add")).Label(tr("添加为快捷输入", "Save as quick input")).Height(26).PaddingX(8).Radius(6).AlignItems(ui.Center).Shrink(0).Disabled(strings.TrimSpace(in.text) == "")
			if add.Hovered() {
				add.Background(p.soft)
			}
			add.Children(func() { ui.Text(c, tr("＋ 快捷输入", "+ Quick input")).FontSize(11.5).TextColor(p.muted) })
			if add.Clicked() && strings.TrimSpace(in.text) != "" && !slices.Contains(x.Quick, in.text) {
				text := in.text
				s.updateDebug(x.ID, func(y *debugSession) { y.Quick = append(slices.Clone(y.Quick), text) })
			}
		})
		hint := tr("输入要发送的内容，⌘↩ 发送", "Type a message; ⌘↩ sends")
		if in.hex {
			hint = tr("输入 16 进制，如 50 49 4e 47 0d 0a", "Type hex, such as 50 49 4e 47 0d 0a")
		} else if !x.NoEscape {
			hint = tr("输入要发送的内容，\\r\\n 按转义发送，⌘↩ 发送", "Type a message; \\r\\n are escapes; ⌘↩ sends")
		}
		ui.TextArea(c.Key("debug-text"), &in.text).Label(tr("要发送的内容", "Message")).Placeholder(hint).FillWidth().Height(64).
			Font(mono).FontSize(12.5).Padding(8, 12).OnTextKey(func(k ui.TextKey) bool {
			if k.Key == ui.KeyEnter && k.Mods&ui.Cmd != 0 {
				if !disabled {
					doSend(in.text)
				}
				return true
			}
			return false
		})
		if in.problem != "" {
			ui.Text(c, in.problem).FontSize(12).TextColor(p.err).Padding(0, 12, 6)
		}
		ui.Row(c).FillWidth().Padding(0, 10, 10, 12).Gap(14).AlignItems(ui.Center).Children(func() {
			repeat := in.repeat
			if ui.Checkbox(c.Key("debug-repeat"), &repeat, tr("定时发送", "Repeat")).Disabled(disabled).Changed() {
				s.setRepeat(in, repeat, func() { doSend(in.text) })
			}
			ui.Row(c).Gap(6).AlignItems(ui.Center).Children(func() {
				ui.Text(c, tr("每", "Every")).FontSize(12.5).TextColor(p.sub)
				ui.TextInput(c.Key("debug-every"), &in.every).Label(tr("发送间隔（秒）", "Interval in seconds")).Width(48).Height(26).Font(mono).FontSize(12)
				ui.Text(c, tr("秒", "s")).FontSize(12.5).TextColor(p.sub)
			})
			keep := in.keep
			if ui.Checkbox(c.Key("debug-keep"), &keep, tr("发送后保留", "Keep after sending")).Changed() {
				in.keep = keep
			}
			raw := x.NoEscape
			if ui.Checkbox(c.Key("debug-raw"), &raw, tr("不处理转义", "No escapes")).Changed() {
				s.updateDebug(x.ID, func(y *debugSession) { y.NoEscape = raw })
			}
			ui.Box(c).Grow(1)
			if target != "" {
				ui.Text(c, target).FontSize(11.5).TextColor(p.muted).SingleLine().MinWidth(0)
			}
			label := tr("发送", "Send")
			if in.repeat {
				label = tr("停止发送", "Stop")
			}
			if button(c.Key("debug-send"), p, label, "", primaryButton, disabled) {
				if in.repeat {
					s.setRepeat(in, false, nil)
				} else {
					doSend(in.text)
				}
			}
		})
	})
}

// setRepeat starts or stops sending the composer's text on an interval.
// The ticks come back to the main thread, which encodes and sends what is
// typed at that moment.
func (s *desktop) setRepeat(in *debugInput, on bool, tick func()) {
	if in.stop != nil {
		in.stop()
		in.stop = nil
	}
	in.repeat = false
	if !on || tick == nil {
		return
	}
	every, err := strconv.ParseFloat(strings.TrimSpace(in.every), 64)
	if err != nil || every < 0.1 {
		in.problem = tr("发送间隔至少 0.1 秒", "The interval must be at least 0.1 seconds")
		return
	}
	in.repeat, in.problem = true, ""
	ctx, cancel := context.WithCancel(s.ctx)
	in.stop = cancel
	tick()
	if s.window == nil {
		return
	}
	go func() {
		t := time.NewTicker(time.Duration(every * float64(time.Second)))
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				s.window.Update(func() {
					if ctx.Err() == nil {
						tick()
					}
				})
			}
		}
	}()
}

// debugHTTPClient is an HTTP client: the request line, the request and the
// response side by side, and the history.
func (s *desktop) debugHTTPClient(c *ui.Context, p palette, x debugSession, r *debugRun, snap debugSnap) {
	d := &s.debug
	busy := snap.state == debugBusy
	ui.Row(c.Key("debug-url")).Shrink(0).Margin(0, 20, 12).Padding(4).Gap(8).Radius(10).Border(1, p.line).AlignItems(ui.Center).Children(func() {
		method := x.Method
		if method == "" {
			method = http.MethodGet
		}
		methods := []string{"GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS"}
		if ui.Select(c.Key("debug-method"), &method, methods).Label(tr("请求方法", "Method")).Height(30).Width(96).Shrink(0).Cursor(ui.CursorPointer).Changed() {
			s.updateDebug(x.ID, func(y *debugSession) { y.Method = method })
		}
		target := x.Address
		hint := "http://localhost:8080/"
		if x.Container != "" {
			hint = "/"
		}
		field := ui.TextInput(c.Key("debug-address"), &target).Label(tr("请求地址", "Request URL")).Placeholder(hint).Width(0).Grow(1).MinWidth(0).
			Height(30).Font(mono).FontSize(13)
		if field.Changed() {
			s.updateDebug(x.ID, func(y *debugSession) { y.Address = target })
		}
		if x.Container != "" {
			ui.Text(c, fmt.Sprintf("%s:%d", x.Container, x.Port)).Font(mono).FontSize(11.5).TextColor(p.muted).Shrink(0)
		}
		if button(c.Key("debug-send"), p, tr("发送", "Send"), "", primaryButton, busy) || field.Submitted() {
			s.startDebug(x)
		}
	})
	var last *httpExchange
	if n := len(snap.exchanges); n > 0 {
		i := n - 1
		if v, ok := d.viewing[x.ID]; ok && v < n {
			i = v
		}
		last = &snap.exchanges[i]
	}
	ui.Row(c.Key("debug-http")).FillWidth().Grow(1).MinHeight(0).Padding(0, 20).Gap(12).AlignItems(ui.Stretch).Children(func() {
		pane := func(key string, head, body func()) {
			ui.Column(c.Key(key)).Width(0).Grow(1).MinWidth(0).Radius(10).Border(1, p.line).Clip().Children(func() {
				ui.Row(c).FillWidth().Height(40).PaddingX(8).Gap(6).AlignItems(ui.Center).BorderWidth(0, 0, 1, 0).BorderColor(p.line).Children(head)
				body()
			})
		}
		pane("debug-request", func() {
			filters(c.Key("debug-req-tab"), p, tr("请求内容", "Request part"), &d.reqTab, []string{"Headers", "Body"}, []int{len(x.Headers)})
		}, func() {
			if d.reqTab == 1 {
				body := x.Body
				if ui.TextArea(c.Key("debug-body"), &body).Label(tr("请求体", "Request body")).Placeholder("{ }").Fill().Font(mono).FontSize(12.5).Padding(10, 12).Changed() {
					s.updateDebug(x.ID, func(y *debugSession) { y.Body = body })
				}
				return
			}
			s.debugHeaders(c, p, x)
		})
		pane("debug-response", func() {
			filters(c.Key("debug-res-tab"), p, tr("响应内容", "Response part"), &d.resTab, []string{tr("响应", "Body"), "Headers"}, nil)
			ui.Box(c).Grow(1)
			if last != nil && last.Err == "" {
				ink := p.ok
				if last.Status >= 400 {
					ink = p.err
				} else if last.Status >= 300 {
					ink = p.warn
				}
				ui.Textf(c, "%d %s", last.Status, http.StatusText(last.Status)).Font(mono).FontSize(12).FontWeight(600).TextColor(ink)
				ui.Text(c, millis(last.Duration)).FontSize(12).TextColor(p.sub)
				ui.Text(c, displaySize(int64(len(last.Body)))).FontSize(12).TextColor(p.sub)
			}
		}, func() {
			switch {
			case last == nil:
				ui.Text(c, tr("发送请求后在这里显示响应", "The response shows here")).FontSize(12.5).TextColor(p.muted).Padding(14)
			case last.Err != "":
				ui.Text(c, last.Err).Font(mono).FontSize(12).TextColor(p.err).Padding(14).Selectable()
			case d.resTab == 1:
				names := make([]string, 0, len(last.Headers))
				for k := range last.Headers {
					names = append(names, k)
				}
				sort.Strings(names)
				ui.Scroll(c.Key("debug-res-headers")).Fill().Children(func() {
					ui.Text(c, last.Proto).Font(mono).FontSize(12).TextColor(p.muted).Padding(8, 14)
					for _, k := range names {
						kv(c.Key(k), p, k, strings.Join(last.Headers[k], ", "))
					}
				})
			default:
				ui.Scroll(c.Key("debug-res-body")).Fill().Background(consoleTheme.Background).Children(func() {
					ui.Text(c, prettyBody(last.Body)).Font(mono).FontSize(12.5).LineHeight(1.6).TextColor(logInk).Padding(12, 14).Selectable()
				})
			}
		})
	})
	ui.Row(c.Key("debug-history")).FillWidth().Shrink(0).Padding(12, 20, 16).Gap(8).AlignItems(ui.Center).Clip().Children(func() {
		if len(snap.exchanges) == 0 {
			return
		}
		ui.Text(c, tr("历史", "History")).FontSize(12).FontWeight(600).TextColor(p.sub).Shrink(0)
		for i := len(snap.exchanges) - 1; i >= 0 && i >= len(snap.exchanges)-6; i-- {
			ex := snap.exchanges[i]
			b := clickable(c.Key(i)).Label(fmt.Sprintf("%s %s %d", ex.Method, ex.URL, ex.Status)).Height(28).PaddingX(10).Gap(8).Radius(8).
				AlignItems(ui.Center).Background(p.soft).Shrink(0).MaxWidth(260)
			if b.Hovered() || last == &snap.exchanges[i] {
				b.Background(p.raised)
			}
			b.Children(func() {
				ui.Text(c, ex.Method).Font(mono).FontSize(11).FontWeight(600).TextColor(protoInk(p, debugHTTP))
				ui.Text(c, urlPath(ex.URL)).Font(mono).FontSize(11.5).TextColor(p.text).SingleLine().MinWidth(0)
				if ex.Err == "" {
					ui.Textf(c, "%d", ex.Status).Font(mono).FontSize(11.5).TextColor(p.sub)
				}
			})
			if b.Clicked() {
				d.viewing[x.ID] = i
			}
		}
	})
	if !busy && len(snap.exchanges) > 0 {
		if v, ok := d.viewing[x.ID]; ok && v >= len(snap.exchanges) {
			delete(d.viewing, x.ID)
		}
	}
}

// millis shows a duration in whole milliseconds.
func millis(d time.Duration) string {
	return fmt.Sprintf("%d ms", d.Milliseconds())
}

// urlPath is the path of a request URL, for the history.
func urlPath(raw string) string {
	if i := strings.Index(raw, "://"); i >= 0 {
		raw = raw[i+3:]
		if j := strings.Index(raw, "/"); j >= 0 {
			return raw[j:]
		}
		return "/"
	}
	return raw
}

// prettyBody shows a response body, indented when it is JSON.
func prettyBody(body []byte) string {
	trimmed := strings.TrimSpace(string(body))
	if strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[") {
		if out, err := indentJSON(body); err == nil {
			return out
		}
	}
	return decodeText(body, "UTF-8")
}

// indentJSON indents a JSON document for reading.
func indentJSON(body []byte) (string, error) {
	var out bytes.Buffer
	if err := json.Indent(&out, body, "", "  "); err != nil {
		return "", err
	}
	return out.String(), nil
}

// kv is one name and value of a headers list.
func kv(c *ui.Context, p palette, name, value string) {
	ui.Row(c).FillWidth().Padding(7, 14).Gap(10).AlignItems(ui.Start).BorderWidth(0, 0, 1, 0).BorderColor(p.line).Children(func() {
		ui.Text(c, name).Font(mono).FontSize(12).TextColor(p.text).Width(150).Shrink(0).SingleLine()
		ui.Text(c, value).Font(mono).FontSize(12).TextColor(p.sub).Width(0).Grow(1).MinWidth(0).Selectable()
	})
}

// debugHeaders edits an HTTP client's request headers in place.
func (s *desktop) debugHeaders(c *ui.Context, p palette, x debugSession) {
	ui.Scroll(c.Key("debug-headers")).Fill().Children(func() {
		for i, h := range x.Headers {
			ui.Row(c.Key(i)).FillWidth().Padding(6, 10).Gap(8).AlignItems(ui.Center).BorderWidth(0, 0, 1, 0).BorderColor(p.line).Children(func() {
				name, value := h.Name, h.Value
				if ui.TextInput(c.Key("name"), &name).Label(tr("Header 名称", "Header name")).Width(140).Height(28).Font(mono).FontSize(12).Changed() {
					s.updateDebug(x.ID, func(y *debugSession) { y.Headers[i].Name = name })
				}
				if ui.TextInput(c.Key("value"), &value).Label(tr("Header 值", "Header value")).Width(0).Grow(1).MinWidth(0).Height(28).Font(mono).FontSize(12).Changed() {
					s.updateDebug(x.ID, func(y *debugSession) { y.Headers[i].Value = value })
				}
				if iconButton(c.Key("remove"), p, "x", tr("删除 Header ", "Remove header ")+h.Name, false).Clicked() {
					s.updateDebug(x.ID, func(y *debugSession) { y.Headers = slices.Delete(slices.Clone(y.Headers), i, i+1) })
				}
			})
		}
		ui.Row(c.Key("add")).Padding(8, 10).Children(func() {
			if button(c.Key("debug-header-add"), p, tr("添加 Header", "Add header"), "plus", ghostButton, false) {
				s.updateDebug(x.ID, func(y *debugSession) { y.Headers = append(slices.Clone(y.Headers), debugHeader{}) })
			}
		})
	})
}

// debugMock is an HTTP mock: its addresses, routes and the requests it
// answered.
func (s *desktop) debugMock(c *ui.Context, p palette, x debugSession, r *debugRun, snap debugSnap) {
	s.askRelayNames(c, x, snap)
	names := s.peerNamer()
	s.debugAddresses(c, p, x)
	ui.Row(c.Key("debug-routes-head")).FillWidth().Shrink(0).Padding(4, 20, 8).Gap(8).AlignItems(ui.Center).Children(func() {
		ui.Text(c, tr("路由", "Routes")).FontSize(13).FontWeight(600).TextColor(p.text)
		ui.Textf(c, "%d", len(x.Routes)).FontSize(12).TextColor(p.muted).Grow(1)
		if button(c.Key("debug-route-add"), p, tr("添加路由", "Add route"), "plus", plainButton, false) {
			s.debug.route = debugRouteDraft{index: -1, method: "GET", path: "/", status: "200", ctype: "application/json"}
			s.form = form{kind: "debug-route"}
		}
	})
	ui.Column(c.Key("debug-routes")).Shrink(0).Margin(0, 20).Radius(10).Border(1, p.line).Clip().Children(func() {
		if len(x.Routes) == 0 {
			ui.Text(c, tr("还没有路由，未匹配的请求返回 404", "No routes yet; unmatched requests get 404")).FontSize(12.5).TextColor(p.muted).Padding(12, 14)
		}
		for i, route := range x.Routes {
			b := clickable(c.Key(i)).Label(route.Method+" "+route.Path).FillWidth().Padding(9, 12).Gap(12).AlignItems(ui.Center).Justify(ui.Start).
				BorderWidth(0, 0, 1, 0).BorderColor(p.line)
			if b.Hovered() {
				b.Background(p.hover)
			}
			b.Children(func() {
				method := strings.ToUpper(route.Method)
				if method == "" {
					method = "ANY"
				}
				ui.Text(c, method).Font(mono).FontSize(11).FontWeight(600).TextColor(methodInk(p, method)).Width(56).Shrink(0)
				ui.Text(c, route.Path).Font(mono).FontSize(12.5).TextColor(p.text).Width(180).Shrink(0).SingleLine()
				status := route.Status
				if status == 0 {
					status = 200
				}
				what := route.Body
				if route.Dir != "" {
					what, status = route.Dir, 200
				}
				ui.Textf(c, "%d", status).Font(mono).FontSize(12).FontWeight(600).TextColor(statusInk(p, status)).Width(40).Shrink(0)
				if route.Delay > 0 {
					ui.Textf(c, tr("延迟 %d ms", "%d ms delay"), route.Delay).FontSize(11.5).TextColor(p.sub).Padding(1, 6).Radius(5).Background(p.soft).Shrink(0)
				}
				ui.Text(c, strings.ReplaceAll(what, "\n", " ")).Font(mono).FontSize(12).TextColor(p.sub).Width(0).Grow(1).MinWidth(0).SingleLine()
				ui.Textf(c, "%d", snap.routeHits[i]).FontSize(12).FontFeatures("tnum").TextColor(p.muted).Shrink(0)
			})
			if b.Clicked() {
				s.debug.route = debugRouteDraft{index: i, method: route.Method, path: route.Path, status: strconv.Itoa(status(route)),
					ctype: route.ContentType, body: route.Body, dir: route.Dir, delay: strconv.Itoa(route.Delay)}
				s.form = form{kind: "debug-route"}
			}
		}
	})
	ui.Row(c.Key("debug-hits-head")).FillWidth().Shrink(0).Padding(14, 20, 8).Gap(8).AlignItems(ui.Center).Children(func() {
		ui.Text(c, tr("请求", "Requests")).FontSize(13).FontWeight(600).TextColor(p.text)
		ui.Textf(c, "%d", len(snap.hits)).FontSize(12).TextColor(p.muted).Grow(1)
		if iconButton(c.Key("debug-hits-clear"), p, "trash", tr("清空请求", "Clear requests"), len(snap.hits) == 0).Clicked() {
			r.clear()
		}
	})
	hits := snap.hits
	state := s.list("debug-hits-" + x.ID)
	state.FollowEnd = true
	if len(hits) == 0 {
		text := tr("等待请求", "Waiting for requests")
		if !r.active() {
			text = tr("未在监听", "Not listening")
		}
		ui.Box(c.Key("debug-hits-empty")).Grow(1).MinHeight(0).Margin(0, 20, 16).Radius(10).Background(consoleTheme.Background).Padding(14).Children(func() {
			ui.Text(c, text).Font(mono).FontSize(12).TextColor(logDimInk)
		})
		return
	}
	size := s.prefs.LogSize + 1
	ui.List(c.Key("debug-hits"), state, len(hits)).Grow(1).MinHeight(0).Margin(0, 20, 16).Padding(8, 0).Radius(10).
		Background(consoleTheme.Background).Rows(func(row ui.ListRow) {
		rc, h := row.Context, hits[row.Index]
		ink := ui.Hex("#4CC38A")
		if h.Status >= 500 || h.Status == 0 {
			ink = logErrInk
		} else if h.Status >= 400 {
			ink = logWarnInk
		}
		ui.Row(rc).FillWidth().PaddingX(14).Gap(12).AlignItems(ui.Center).Children(func() {
			ui.Text(rc, h.At.Format("15:04:05")).Font(mono).FontSize(size).LineHeight(1.7).TextColor(logGutter).Shrink(0)
			ui.Text(rc, h.Method).Font(mono).FontSize(size).TextColor(logInk).Width(56).Shrink(0)
			ui.Text(rc, h.Path).Font(mono).FontSize(size).TextColor(logInk).Width(0).Grow(1).MinWidth(0).SingleLine()
			ui.Text(rc, names(h.Peer)).Font(mono).FontSize(size).TextColor(logWarnInk).Shrink(0).MaxWidth(160).SingleLine()
			ui.Textf(rc, "%d", h.Status).Font(mono).FontSize(size).TextColor(ink).Width(36).Shrink(0)
			ui.Text(rc, millis(h.Duration)).Font(mono).FontSize(size - 1).TextColor(logGutter).Width(64).Shrink(0).TextAlign(ui.End)
		})
	})
}

// status is a route's status with the default filled in.
func status(route debugRoute) int {
	if route.Status == 0 {
		return http.StatusOK
	}
	return route.Status
}

// methodInk colors an HTTP method.
func methodInk(p palette, method string) ui.Color {
	switch method {
	case "GET", "HEAD":
		return p.ok
	case "POST", "PUT", "PATCH":
		return p.accent
	case "DELETE":
		return p.err
	}
	return p.sub
}

// statusInk colors an HTTP status.
func statusInk(p palette, code int) ui.Color {
	switch {
	case code >= 500:
		return p.err
	case code >= 400:
		return p.warn
	}
	return p.ok
}

// updateDebug changes a saved session and writes the file. A running
// session sees the change at once through its live copy: a server's reply
// settings and a mock's routes apply without dropping its connections;
// anything else applies on its next start.
func (s *desktop) updateDebug(id string, change func(*debugSession)) {
	for i := range s.debug.sessions {
		if s.debug.sessions[i].ID != id {
			continue
		}
		change(&s.debug.sessions[i])
		s.saveDebug()
		if r := s.debug.runs[id]; r != nil && r.active() {
			x := s.debug.sessions[i]
			r.mu.Lock()
			r.live = &x
			r.mu.Unlock()
		}
		return
	}
}

// duplicateDebug saves a copy of a session under a new name.
func (s *desktop) duplicateDebug(x debugSession) {
	y := x
	y.ID = newDebugID()
	y.Name = x.Name + tr(" 副本", " copy")
	y.Quick = slices.Clone(x.Quick)
	y.Headers = slices.Clone(x.Headers)
	y.Routes = slices.Clone(x.Routes)
	s.debug.sessions = append(s.debug.sessions, y)
	s.debug.cur = y.ID
	s.saveDebug()
}

// deleteDebug stops a session and forgets it, after asking.
func (s *desktop) deleteDebug(x debugSession) {
	s.pending = &confirmation{title: fmt.Sprintf(tr("删除会话 %s？", "Delete session %s?"), x.Name), ok: tr("删除", "Delete"), run: func() {
		if r := s.debug.runs[x.ID]; r != nil {
			go r.stop()
			delete(s.debug.runs, x.ID)
		}
		if in := s.debug.inputs[x.ID]; in != nil && in.stop != nil {
			in.stop()
		}
		delete(s.debug.inputs, x.ID)
		s.debug.sessions = slices.DeleteFunc(s.debug.sessions, func(y debugSession) bool { return y.ID == x.ID })
		s.saveDebug()
	}}
}

// exportDebugLog saves a session's messages as text.
func (s *desktop) exportDebugLog(x debugSession, r *debugRun) {
	snap := r.snapshot()
	names := s.peerNamer()
	var b strings.Builder
	for _, l := range snap.lines {
		dir := map[byte]string{lineSent: "->", lineReceived: "<-", lineNote: "--", lineError: "!!"}[l.Dir]
		peer := ""
		if l.Peer != "" {
			peer = " [" + names(l.Peer) + "]"
		}
		text := l.Note
		if len(l.Data) > 0 {
			text = decodeText(l.Data, x.Encoding)
		}
		fmt.Fprintf(&b, "%s %s%s %s\n", l.At.Format("2006-01-02 15:04:05.000"), dir, peer, text)
	}
	name := strings.NewReplacer("/", "-", " ", "-", ":", "-").Replace(x.Name) + ".log"
	content := b.String()
	go func() {
		path := s.chooseFile(true, name, fileKind{tr("导出日志", "Export log"), tr("导出", "Export"), tr("文本", "Text"), []string{"log", "txt"}})
		if path == "" {
			return
		}
		err := os.WriteFile(path, []byte(content), 0o600)
		s.onMain(func() {
			if err != nil {
				s.notices = append(s.notices, tr("导出失败：", "Export failed: ")+err.Error())
				return
			}
			s.notices = append(s.notices, tr("已导出 ", "Exported ")+path)
		})
	}()
}

// newDebugID is a random session ID.
func newDebugID() string {
	return strings.ReplaceAll(newUUID(), "-", "")[:12]
}

// draftOf fills the dialog from a saved session, to edit it.
func draftOf(x debugSession) debugDraft {
	d := debugDraft{editing: x.ID, kind: x.Kind, proto: x.Proto, name: x.Name, note: x.Note, bindAll: x.BindAll, greeting: x.Greeting,
		tls: x.TLS, cert: x.CertFile, key: x.KeyFile, auto: x.AutoListen, via: x.Via, insecure: x.Insecure}
	if x.Kind == debugServer {
		d.port = strconv.Itoa(x.Port)
		return d
	}
	if x.Container != "" {
		d.target = fmt.Sprintf("%s:%d", x.Container, x.Port)
		d.path = x.Address
	} else {
		d.byAddress, d.address = true, x.Address
	}
	return d
}

// openDebugForm opens the new-session dialog, or the edit dialog when the
// draft names a session.
func (s *desktop) openDebugForm(d debugDraft) {
	if d.kind == "" {
		d.kind, d.proto, d.via, d.port = debugClient, debugTCP, "net", "9000"
	}
	if d.via == "" {
		d.via = "net"
	}
	s.debug.draft = d
	s.form = form{kind: "debug"}
	s.readListening()
}

// readListening asks the engine which TCP ports the running containers
// listen on, so the dialog can offer unpublished ones.
func (s *desktop) readListening() {
	var running []container
	for _, x := range s.containers {
		if x.State == "running" && !x.Helper && x.Machine == "" {
			running = append(running, x)
		}
	}
	s.run(10*time.Second, func(ctx context.Context) func() {
		found := map[string][]int{}
		for _, x := range running {
			var ports struct {
				TCP []int `json:"tcp"`
			}
			if s.engine.get(context.WithValue(ctx, backgroundRead{}, true), "/fabo/containers/"+x.ID+"/listening", &ports) == nil {
				found[x.Name] = ports.TCP
			}
		}
		return func() { s.debug.listening = found }
	})
}

// openDebugFor opens the new-session dialog aimed at a container's port.
func (s *desktop) openDebugFor(x container, port portMap) {
	proto := debugTCP
	if port.Type == debugUDP {
		proto = debugUDP
	}
	s.openDebugForm(debugDraft{kind: debugClient, proto: proto, target: fmt.Sprintf("%s:%d", x.Name, port.Private), via: "net"})
}

// debugPorts are the container ports a client of proto can target.
func (s *desktop) debugPorts(proto string) []debugPortChoice {
	want := debugTCP
	if proto == debugUDP {
		want = debugUDP
	}
	var out []debugPortChoice
	for _, x := range s.containers {
		if x.State != "running" || x.Helper || x.Machine != "" {
			continue
		}
		seen := map[int]bool{}
		for _, pm := range x.Ports {
			kind := pm.Type
			if kind == "" {
				kind = debugTCP
			}
			if kind != want || seen[pm.Private] {
				continue
			}
			seen[pm.Private] = true
			ip := ""
			for _, n := range x.Networks {
				if n.IP != "" {
					ip = n.IP
					break
				}
			}
			out = append(out, debugPortChoice{container: x, port: pm.Private, public: pm.Public, ip: ip})
		}
		if want == debugTCP {
			for _, port := range s.debug.listening[x.Name] {
				if !seen[port] {
					seen[port] = true
					out = append(out, debugPortChoice{container: x, port: port, ip: containerIP(x)})
				}
			}
		}
		// A container with no port to list is still offered, with its
		// port typed below.
		if len(seen) == 0 {
			out = append(out, debugPortChoice{container: x, ip: containerIP(x)})
		}
	}
	return out
}

// containerIP is a container's first network address.
func containerIP(x container) string {
	for _, n := range x.Networks {
		if n.IP != "" {
			return n.IP
		}
	}
	return ""
}

// debugPortChoice is one container port the dialog offers.
type debugPortChoice struct {
	container    container
	port, public int
	ip           string
}

// key names a port choice as the draft keeps it.
func (c debugPortChoice) key() string { return fmt.Sprintf("%s:%d", c.container.Name, c.port) }

// debugForm is the new-session dialog's body; it reports a submission.
func (s *desktop) debugForm(c *ui.Context, p palette) bool {
	d := &s.debug.draft
	submitted := false
	if d.editing == "" {
		ui.Grid(c.Key("debug-types")).Columns(5).Gap(8).FillWidth().Children(func() {
			for _, kind := range []string{debugClient, debugServer} {
				ui.Text(c.Key(kind+"-title"), map[string]string{debugClient: tr("客户端", "Client"), debugServer: tr("服务端", "Server")}[kind]).
					FontSize(12).FontWeight(600).TextColor(p.sub).AlignSelf(ui.Center)
				for _, proto := range []string{debugTCP, debugUDP, debugWS, debugHTTP} {
					name := protoName(proto)
					if proto == debugHTTP && kind == debugServer {
						name = "HTTP Mock"
					}
					on := d.kind == kind && d.proto == proto
					b := clickable(c.Key(kind+proto)).Label(map[string]string{debugClient: tr("客户端 ", "Client "), debugServer: tr("服务端 ", "Server ")}[kind]+name).
						Height(36).PaddingX(12).Radius(9).AlignItems(ui.Center).Justify(ui.Start).Border(1.5, p.line)
					switch {
					case on:
						b.Border(1.5, p.accent).Background(p.accent.Alpha(0.06))
					case b.Hovered():
						b.Background(p.hover)
					}
					b.Children(func() { ui.Text(c, name).FontSize(13).FontWeight(600).TextColor(p.text).SingleLine() })
					if b.Clicked() {
						d.kind, d.proto = kind, proto
					}
				}
			}
		})
	}
	if d.kind == debugServer {
		ui.Row(c.Key("debug-server-fields")).FillWidth().Gap(12).Children(func() {
			ui.Box(c).Width(0).Grow(1).Children(func() {
				submitted = input(c, p, "port", tr("端口", "Port"), "9000", &d.port, d.editing == "").Submitted() || submitted
			})
			ui.Box(c).Width(0).Grow(1).Children(func() {
				submitted = input(c, p, "name", tr("名称", "Name"), protoName(d.proto)+" :"+d.port, &d.name, false).Submitted() || submitted
			})
		})
		ui.Row(c.Key("debug-bind")).FillWidth().Gap(12).AlignItems(ui.End).Children(func() {
			ui.Column(c).Gap(5).Children(func() {
				ui.Text(c, tr("监听", "Listen on")).FontSize(11.5).TextColor(p.muted)
				bind := 0
				if d.bindAll {
					bind = 1
				}
				if filters(c.Key("debug-bind-all"), p, tr("监听范围", "Listen on"), &bind, []string{tr("仅本机", "This Mac"), tr("所有网卡", "All interfaces")}, nil) {
					d.bindAll = bind == 1
				}
			})
			// UDP has no connection to greet.
			if d.proto == debugTCP || d.proto == debugWS {
				ui.Box(c).Width(0).Grow(1).Children(func() {
					input(c, p, "greeting", tr("初始消息", "Greeting"), tr("连接时发送，可留空", "Sent on connect; optional"), &d.greeting, false)
				})
			}
		})
		if d.proto == debugWS || d.proto == debugHTTP {
			ui.Checkbox(c.Key("debug-tls"), &d.tls, tr("启用 TLS（自签名证书，可换成自己的证书）", "Use TLS (self-signed unless you give a certificate)"))
			if d.tls {
				ui.Row(c.Key("debug-cert")).FillWidth().Gap(12).Children(func() {
					ui.Box(c).Width(0).Grow(1).Children(func() {
						input(c, p, "cert", tr("证书（PEM 路径）", "Certificate (PEM path)"), tr("留空用自签名", "Empty for self-signed"), &d.cert, false)
					})
					ui.Box(c).Width(0).Grow(1).Children(func() {
						input(c, p, "key", tr("私钥（PEM 路径）", "Key (PEM path)"), tr("留空用自签名", "Empty for self-signed"), &d.key, false)
					})
				})
			}
		}
		ui.Text(c, "host.docker.internal:"+d.port).Font(mono).FontSize(12).TextColor(p.sub)
		ui.Checkbox(c.Key("debug-auto-listen"), &d.auto, tr("启动 Fabo 时自动监听", "Listen when Fabo opens"))
	} else {
		ui.Row(c.Key("debug-target-head")).FillWidth().AlignItems(ui.Center).Children(func() {
			ui.Text(c, tr("目标", "Target")).FontSize(11.5).TextColor(p.muted).Grow(1)
			by := 0
			if d.byAddress {
				by = 1
			}
			if filters(c.Key("debug-target-by"), p, tr("目标类型", "Target kind"), &by, []string{tr("容器", "Container"), tr("地址", "Address")}, nil) {
				d.byAddress = by == 1
			}
		})
		if d.byAddress {
			hint := "127.0.0.1:8080"
			switch d.proto {
			case debugWS:
				hint = "ws://127.0.0.1:8080/"
			case debugHTTP:
				hint = "http://127.0.0.1:8080/"
			}
			submitted = input(c, p, "address", tr("地址", "Address"), hint, &d.address, true).Submitted() || submitted
		} else {
			choices := s.debugPorts(d.proto)
			var pick *debugPortChoice
			for i := range choices {
				if choices[i].key() == d.target {
					pick = &choices[i]
				}
			}
			// A target not in the lists (a typed port, or one no longer
			// listening) keeps its container and moves its port to the
			// typed field.
			if pick == nil && d.target != "" {
				name, port, _ := strings.Cut(d.target, ":")
				for i := range choices {
					if choices[i].container.Name == name {
						pick = &choices[i]
						if d.other == "" {
							d.other = port
						}
						d.target = pick.key()
						break
					}
				}
			}
			if pick == nil && len(choices) > 0 {
				pick = &choices[0]
				d.target = pick.key()
			}
			ui.Scroll(c.Key("debug-ports")).FillWidth().MaxHeight(200).Radius(10).Border(1, p.line).Children(func() {
				if len(choices) == 0 {
					ui.Text(c, tr("没有运行中的容器端口", "No running container ports")).FontSize(12.5).TextColor(p.muted).Padding(12)
				}
				for _, ch := range choices {
					on := pick != nil && pick.key() == ch.key()
					b := clickable(c.Key(ch.key())).Label(ch.key()).FillWidth().Padding(8, 12).Gap(10).AlignItems(ui.Center).Justify(ui.Start).
						BorderWidth(0, 0, 1, 0).BorderColor(p.line)
					switch {
					case on:
						b.Background(p.accent.Alpha(0.07))
					case b.Hovered():
						b.Background(p.hover)
					}
					b.Children(func() {
						radio(c, p, on)
						ui.Column(c).Width(0).Grow(1).MinWidth(0).Children(func() {
							ui.Text(c, ch.container.Name).FontSize(13).FontWeight(600).TextColor(p.text).SingleLine()
						})
						mapped := tr("未映射", "Not published")
						if ch.public > 0 {
							mapped = fmt.Sprintf("→ localhost:%d", ch.public)
						}
						if ch.port == 0 {
							ui.Text(c, tr("在下方填写端口", "Type the port below")).FontSize(12).TextColor(p.muted).Shrink(0)
						} else {
							ui.Textf(c, "%d/%s  %s", ch.port, map[bool]string{true: "udp", false: "tcp"}[d.proto == debugUDP], mapped).Font(mono).FontSize(12).TextColor(p.sub).Shrink(0)
						}
					})
					if b.Clicked() {
						d.target = ch.key()
					}
				}
			})
			// A port the lists do not show, typed on the chosen container.
			ui.Row(c.Key("debug-other")).FillWidth().Gap(8).AlignItems(ui.Center).Children(func() {
				ui.Text(c, tr("其他端口", "Other port")).FontSize(11.5).TextColor(p.muted).Shrink(0)
				ui.TextInput(c.Key("debug-other-port"), &d.other).Label(tr("其他端口", "Other port")).Placeholder("5432").Width(90).Height(28).Font(mono).FontSize(12)
				if pick != nil && strings.TrimSpace(d.other) != "" {
					ui.Text(c, fmt.Sprintf(tr("连 %s 的这个端口", "on %s"), pick.container.Name)).FontSize(12).TextColor(p.sub).SingleLine().MinWidth(0)
				}
			})
			if pick != nil && strings.TrimSpace(d.other) != "" {
				// A typed port is reached through the container network.
				d.via = "net"
			}
			if pick != nil {
				if pick.public == 0 && d.via == "host" {
					d.via = "net"
				}
				ui.Text(c, tr("连接方式", "Connect through")).FontSize(11.5).TextColor(p.muted)
				ui.Row(c.Key("debug-via")).FillWidth().Gap(8).Children(func() {
					option := func(key, title, detail string, disabled bool) {
						on := d.via == key
						b := clickable(c.Key(key)).Label(title).Width(0).Grow(1).Height(36).PaddingX(10).Gap(8).Radius(8).AlignItems(ui.Center).
							Border(1.5, p.line).Disabled(disabled)
						if on {
							b.Border(1.5, p.accent)
						}
						if disabled {
							b.Opacity(0.45)
						}
						b.Children(func() {
							radio(c, p, on)
							ui.Text(c, title).FontSize(12.5).TextColor(p.text).Grow(1)
							ui.Text(c, detail).Font(mono).FontSize(12).TextColor(p.sub)
						})
						if b.Clicked() && !disabled {
							d.via = key
						}
					}
					port := strconv.Itoa(pick.port)
					if other := strings.TrimSpace(d.other); other != "" {
						port = other
					}
					option("net", tr("容器网络", "Container network"), net.JoinHostPort(pick.ip, port), false)
					hostDetail := tr("未映射", "Not published")
					if pick.public > 0 {
						hostDetail = fmt.Sprintf("localhost:%d", pick.public)
					}
					option("host", tr("映射端口", "Published port"), hostDetail, pick.public == 0)
				})
			}
			if d.proto == debugWS || d.proto == debugHTTP {
				input(c, p, "path", tr("路径", "Path"), "/", &d.path, false)
			}
		}
		if d.proto == debugWS || d.proto == debugHTTP {
			ui.Checkbox(c.Key("debug-insecure"), &d.insecure, tr("不校验服务器证书（自签名证书时使用）", "Skip checking the server's certificate (for self-signed ones)"))
		}
		submitted = input(c, p, "name", tr("名称", "Name"), tr("自动生成", "Generated"), &d.name, false).Submitted() || submitted
	}
	input(c, p, "note", tr("备注", "Note"), tr("可留空", "Optional"), &d.note, false)
	return submitted
}

// radio is a round choice mark.
func radio(c *ui.Context, p palette, on bool) {
	b := ui.Box(c).Size(16, 16).Shrink(0).Radius(8)
	if on {
		b.Border(5, p.accent)
	} else {
		b.Border(1.5, p.muted.Alpha(0.6))
	}
}

// createDebug checks the dialog and saves the session, starting it.
func (s *desktop) createDebug() bool {
	d := &s.debug.draft
	x := debugSession{ID: d.editing, Kind: d.kind, Proto: d.proto, Name: strings.TrimSpace(d.name), Note: strings.TrimSpace(d.note)}
	if old, i := s.debugSession(d.editing); i >= 0 {
		x = old
		x.Name, x.Note = strings.TrimSpace(d.name), strings.TrimSpace(d.note)
	}
	if x.Kind == debugServer {
		port, err := strconv.Atoi(strings.TrimSpace(d.port))
		if err != nil {
			port = 0
		}
		x.Port, x.BindAll, x.Greeting, x.TLS, x.AutoListen = port, d.bindAll, d.greeting, d.tls, d.auto
		x.CertFile, x.KeyFile = strings.TrimSpace(d.cert), strings.TrimSpace(d.key)
		if x.Name == "" {
			x.Name = fmt.Sprintf("%s :%d", protoName(x.Proto), port)
		}
		if x.Proto == debugHTTP && x.Routes == nil {
			x.Routes = []debugRoute{{Method: "GET", Path: "/", Status: 200, ContentType: "application/json", Body: `{"ok":true}`}}
		}
	} else if d.byAddress {
		x.Address, x.Container, x.Port, x.Via, x.Insecure = strings.TrimSpace(d.address), "", 0, "", d.insecure
		if x.Name == "" {
			x.Name = x.Address
		}
	} else {
		name, port, _ := strings.Cut(d.target, ":")
		if other := strings.TrimSpace(d.other); other != "" {
			port, d.via = other, "net"
		}
		x.Container, x.Via, x.Address, x.Insecure = name, d.via, strings.TrimSpace(d.path), d.insecure
		x.Port, _ = strconv.Atoi(port)
		if x.Name == "" {
			x.Name = fmt.Sprintf("%s:%d", name, x.Port)
		}
	}
	if x.Kind == debugClient && x.Proto == debugHTTP && x.Method == "" {
		x.Method = http.MethodGet
	}
	if err := x.valid(); err != nil {
		s.form.problem = err.Error()
		return false
	}
	if x.Kind == debugServer {
		for _, y := range s.debug.sessions {
			if y.Kind == debugServer && y.Port == x.Port && y.ID != x.ID && (y.Proto == debugUDP) == (x.Proto == debugUDP) {
				s.form.problem = fmt.Sprintf(tr("会话 %s 已使用端口 %d", "Session %s already uses port %d"), y.Name, x.Port)
				return false
			}
		}
	}
	if x.ID == "" {
		x.ID = newDebugID()
		s.debug.sessions = append(s.debug.sessions, x)
		s.debug.cur = x.ID
		s.saveDebug()
		if x.Proto != debugHTTP || x.Kind == debugServer {
			s.startDebug(x)
		}
		return true
	}
	s.updateDebug(x.ID, func(y *debugSession) { *y = x })
	return true
}

// debugRouteForm is the route dialog's body.
func (s *desktop) debugRouteForm(c *ui.Context, p palette) bool {
	f := &s.debug.route
	submitted := false
	ui.Row(c.Key("debug-route-line")).FillWidth().Gap(10).AlignItems(ui.End).Children(func() {
		ui.Column(c).Gap(5).Children(func() {
			ui.Text(c, tr("方法", "Method")).FontSize(11.5).TextColor(p.muted)
			ui.Select(c.Key("debug-route-method"), &f.method, []string{"GET", "POST", "PUT", "PATCH", "DELETE", "ANY"}).Label(tr("方法", "Method")).Height(30).Width(100)
		})
		ui.Box(c).Width(0).Grow(1).Children(func() {
			submitted = input(c, p, "route-path", tr("路径", "Path"), "/v1/items/:id", &f.path, true).Submitted() || submitted
		})
		ui.Box(c).Width(80).Children(func() {
			submitted = input(c, p, "route-status", tr("状态码", "Status"), "200", &f.status, false).Submitted() || submitted
		})
	})
	ui.Row(c.Key("debug-route-meta")).FillWidth().Gap(10).Children(func() {
		ui.Box(c).Width(0).Grow(1).Children(func() {
			input(c, p, "route-type", "Content-Type", "application/json", &f.ctype, false)
		})
		ui.Box(c).Width(120).Children(func() {
			input(c, p, "route-delay", tr("延迟（毫秒）", "Delay (ms)"), "0", &f.delay, false)
		})
	})
	ui.Column(c.Key("debug-route-body")).FillWidth().Gap(5).Children(func() {
		ui.Text(c, tr("响应内容，可用 {{path.id}} {{uuid}} {{rand}}", "Body; {{path.id}} {{uuid}} {{rand}} are filled in")).FontSize(11.5).TextColor(p.muted)
		ui.TextArea(c.Key("route-body"), &f.body).Label(tr("响应内容", "Body")).FillWidth().Height(110).Font(mono).FontSize(12)
	})
	input(c, p, "route-dir", tr("或者返回目录里的文件（路径以 /* 结尾）", "Or serve files from a folder (path ends in /*)"), tr("可留空", "Optional"), &f.dir, false)
	if f.index >= 0 {
		if button(c.Key("debug-route-delete"), p, tr("删除路由", "Delete route"), "trash", removeButton, false) {
			index := f.index
			s.updateDebug(s.debug.cur, func(y *debugSession) { y.Routes = slices.Delete(slices.Clone(y.Routes), index, index+1) })
			s.form = form{}
		}
	}
	return submitted
}

// saveRoute checks the route dialog and saves the route.
func (s *desktop) saveRoute() bool {
	f := &s.debug.route
	code, err := strconv.Atoi(strings.TrimSpace(f.status))
	if err != nil || code < 100 || code > 599 {
		s.form.problem = tr("状态码要是 100–599", "The status must be from 100 to 599")
		return false
	}
	delay := 0
	if strings.TrimSpace(f.delay) != "" {
		if delay, err = strconv.Atoi(strings.TrimSpace(f.delay)); err != nil || delay < 0 {
			s.form.problem = tr("延迟要是非负整数", "The delay must be a whole number of milliseconds")
			return false
		}
	}
	path := strings.TrimSpace(f.path)
	if !strings.HasPrefix(path, "/") {
		s.form.problem = tr("路径要以 / 开头", "The path must start with /")
		return false
	}
	route := debugRoute{Method: f.method, Path: path, Status: code, ContentType: strings.TrimSpace(f.ctype), Body: f.body, Delay: delay, Dir: strings.TrimSpace(f.dir)}
	index := f.index
	s.updateDebug(s.debug.cur, func(y *debugSession) {
		routes := slices.Clone(y.Routes)
		if index >= 0 && index < len(routes) {
			routes[index] = route
		} else {
			routes = append(routes, route)
		}
		y.Routes = routes
	})
	return true
}
