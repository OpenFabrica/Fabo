package main

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/egoist/mygo/ui"
)

const (
	stripHeight      = 40
	sidebarOpenWidth = 208
	sidebarIconWidth = 52
)

// rowHeight is the height of a table row at the chosen density.
func (s *desktop) rowHeight() float32 {
	if s.prefs.Density == "cozy" {
		return 36
	}
	return 30
}

// list returns the scroll state of a named list, which must outlive a frame.
func (s *desktop) list(name string) *ui.ListState {
	if s.lists[name] == nil {
		s.lists[name] = &ui.ListState{}
	}
	return s.lists[name]
}

// view builds the whole window: title strip, sidebar, the page, status bar,
// then the dialogs that float above them.
func (s *desktop) view(c *ui.Context) {
	lang = s.prefs.language(s.locale)
	mono = "monospace"
	if s.prefs.Mono != "" {
		mono = s.prefs.Mono
	}
	c.SetZoom(s.prefs.Scale)
	dark := c.Theme().Dark
	switch s.prefs.Theme {
	case "dark":
		dark = true
	case "light":
		dark = false
	}
	s.dark = dark
	p := colors(dark, s.prefs.Accent)
	theme := *c.Theme()
	theme.Dark = dark
	theme.Font = s.prefs.Font
	theme.Accent, theme.AccentText, theme.Selection = p.accent, p.onAccent, p.accent.Alpha(0.25)
	theme.Background, theme.Surface, theme.Border = p.bg, p.panel, p.line
	theme.Text, theme.TextMuted = p.text, p.muted
	c.SetTheme(&theme)

	s.visible = s.visible[:0]
	ui.Column(c).Fill().Background(p.bg).Children(func() {
		s.strip(c, p)
		ui.Row(c.Key("body")).FillWidth().Grow(1).MinHeight(0).AlignItems(ui.Stretch).Children(func() {
			s.sidebar(c, p)
			ui.Column(c.Key("page-" + s.page)).Grow(1).Width(0).MinWidth(0).Children(func() { s.pageBody(c, p) })
		})
		s.statusBar(c, p)
	})
	s.confirm(c, p)
	s.dialog(c, p)
	for _, notice := range s.notices {
		c.Toast(notice)
	}
	s.notices = nil
	s.keys(c)
}

// keys handles the window's shortcuts: ⌘K to search, ⌘B to fold the sidebar,
// and the arrows and Escape for the selection of the list on screen.
func (s *desktop) keys(c *ui.Context) {
	c.OnWindowShortcut(ui.Cmd, ui.KeyK, func() { s.searchBox.Focus() })
	c.OnWindowShortcut(ui.Cmd, ui.KeyB, s.toggleSidebar)
	if s.pending != nil || s.form.kind != "" || c.FocusTakesText() {
		return
	}
	if c.Shortcut(0, ui.KeyEscape) {
		s.selectRow("")
	}
	step := 0
	if c.Shortcut(0, ui.KeyDown) {
		step = 1
	} else if c.Shortcut(0, ui.KeyUp) {
		step = -1
	}
	if step == 0 || len(s.visible) == 0 {
		return
	}
	at := -1
	for i, id := range s.visible {
		if id == s.selected[s.page] {
			at = i
		}
	}
	at = min(len(s.visible)-1, max(0, at+step))
	s.selectRow(s.visible[at])
}

func (s *desktop) toggleSidebar() {
	s.prefs.NavCollapsed = !s.prefs.NavCollapsed
	s.savePrefs()
}

// searchable reports whether the page has a list the title bar's field filters.
func searchable(page string) bool {
	return page == containersPage || page == imagesPage || page == volumesPage || page == networksPage
}

// strip is the window's title bar: sidebar switch, page name, the search
// field, and the engine's state while the sidebar is folded.
func (s *desktop) strip(c *ui.Context, p palette) {
	left := max(14, c.TitleBar().Left+6)
	ui.Row(c.Key("strip")).FillWidth().Height(stripHeight).Shrink(0).Padding(0, 10, 0, left).Gap(10).AlignItems(ui.Center).
		Background(p.panel).BorderWidth(0, 0, 1, 0).BorderColor(p.line).DragWindow().Children(func() {
		toggle := tr("收起侧栏", "Hide sidebar")
		if s.prefs.NavCollapsed {
			toggle = tr("展开侧栏", "Show sidebar")
		}
		if iconButton(c.Key("sidebar-toggle"), p, "sidebar", toggle, false).Clicked() {
			s.toggleSidebar()
		}
		ui.Image(c, markIcon).Size(16, 16).Shrink(0)
		ui.Text(c, pageTitle(s.page)).FontSize(12.5).FontWeight(600).TextColor(p.text).Shrink(0)
		ui.Box(c).Grow(1)
		if searchable(s.page) {
			hint := tr("搜索", "Search ") + strings.ToLower(pageTitle(s.page))
			field := ui.SearchField(c.Key("search-"+s.page), &s.search).Placeholder(hint + "  ⌘K").Label(tr("搜索", "Search")).
				Width(380).Shrink(1).MinWidth(140).Height(26).Radius(5).Bind(&s.searchBox)
			if field.Changed() {
				s.list(s.page).ScrollTo(0, ui.Start)
			}
		}
		ui.Box(c).Grow(1)
		if s.prefs.NavCollapsed {
			s.engineLine(c, p)
		}
	})
}

// engineWords names the engine's state.
func (s *desktop) engineWords() (kind, text string) {
	switch s.engineState() {
	case "running":
		return "running", tr("引擎运行中", "Engine running")
	case "starting":
		return "busy", tr("引擎启动中…", "Engine starting…")
	case "asleep":
		return "stopped", tr("引擎休眠中", "Engine asleep")
	case "stopped":
		return "stopped", tr("引擎已停止", "Engine stopped")
	}
	return "failed", tr("引擎未连接", "Engine unavailable")
}

// engineSwitch is the button that starts or stops the engine. An engine that
// runs directly on Linux has no VM to control and shows none.
func (s *desktop) engineSwitch(c *ui.Context, p palette) {
	if s.info.KiriVMControls != nil && !*s.info.KiriVMControls {
		return
	}
	switch s.engineState() {
	case "running":
		if iconButton(c.Key("engine-stop"), p, "power", tr("停止引擎", "Stop engine"), false).Clicked() {
			s.stopEngine()
		}
	case "stopped", "asleep":
		if iconButton(c.Key("engine-start"), p, "play", tr("启动引擎", "Start engine"), false).Clicked() {
			s.do("engine", engineAction{http.MethodPost, "/kiri/wake", nil}, tr("引擎已启动", "Engine started"))
		}
	}
}

// engineLine is the one-line engine state of the title bar.
func (s *desktop) engineLine(c *ui.Context, p palette) {
	kind, text := s.engineWords()
	ui.Row(c.Key("engine-line")).Shrink(0).Gap(7).AlignItems(ui.Center).Children(func() {
		dot(c, p, kind)
		if kind == "running" {
			used, total := s.memoryUse()
			text += fmt.Sprintf(tr(" · %d 核", " · %d CPU"), s.cores())
			if total > 0 {
				text += fmt.Sprintf(" · %.1f/%.0f GB", float64(used)/(1<<30), float64(total)/(1<<30))
			}
		}
		ui.Text(c, text).FontSize(12).FontFeatures("tnum").TextColor(p.muted)
		s.engineSwitch(c, p)
	})
}

// sidebar is the navigation. Open, it names each page with its count and
// carries the engine card; folded, it shows icons only.
func (s *desktop) sidebar(c *ui.Context, p palette) {
	open := !s.prefs.NavCollapsed
	width, pad := float32(sidebarIconWidth), float32(8)
	if open {
		width = sidebarOpenWidth
	}
	counts := map[string]int{containersPage: len(s.containers), imagesPage: len(s.images), volumesPage: len(s.volumes), networksPage: len(s.networks)}
	side := ui.Column(c.Key("sidebar")).Width(width).Shrink(0).Padding(pad).Gap(2).Justify(ui.Start).
		Background(p.panel).BorderWidth(0, 1, 0, 0).BorderColor(p.line)
	if !open {
		side.AlignItems(ui.Center)
	}
	side.Children(func() {
		for i, n := range navItems {
			if n.page == settingsPage {
				ui.Box(c).Grow(1)
				if open {
					s.engineCard(c, p)
				}
			}
			// Section breaks: before the Docker pages and before the
			// reserved ones.
			if i == 1 || i == 6 {
				if !open {
					ui.Box(c).Width(24).Height(1).Shrink(0).Margin(6, 0).Background(p.line)
				} else {
					title := "Docker"
					if i == 6 {
						title = tr("扩展", "Extensions")
					}
					ui.Text(c, title).FontSize(10.5).FontWeight(600).LetterSpacing(0.6).TextColor(p.muted).Padding(12, 10, 4)
				}
			}
			count, counted := counts[n.page]
			if s.navButton(c.Key("nav-"+n.page), p, n, open, count, counted) {
				s.navigate(n.page)
			}
		}
	})
}

// navButton is one sidebar entry.
func (s *desktop) navButton(c *ui.Context, p palette, n navItem, open bool, count int, counted bool) bool {
	name := tr(n.zh, n.en)
	b := clickable(c).Label(name).Radius(6).Shrink(0).AlignItems(ui.Center)
	if open {
		b.FillWidth().Height(30).PaddingX(10).Gap(9).Justify(ui.Start)
	} else {
		b.Size(36, 34).Justify(ui.Center).Tooltip(name)
	}
	on := s.page == n.page
	ink, text := p.muted, p.text
	switch {
	case on:
		b.Background(p.raised)
		ink, text = p.accent, p.accent
	case b.Hovered():
		b.Background(p.hover)
		ink = p.text
	}
	if n.soon && !on {
		ink, text = p.muted.Alpha(0.6), p.muted
	}
	b.Children(func() {
		size := float32(18)
		if open {
			size = 16
		}
		ui.Icon(c, icons[n.icon]).Size(size, size).Shrink(0).TextColor(ink)
		if !open {
			return
		}
		weight := 400
		if on {
			weight = 600
		}
		ui.Text(c, name).FontSize(12.5).FontWeight(weight).TextColor(text).SingleLine().Grow(1).MinWidth(0)
		if counted {
			ui.Textf(c, "%d", count).FontSize(11).FontFeatures("tnum").TextColor(p.muted)
		}
	})
	return b.Clicked()
}

// engineCard sits at the foot of the open sidebar: state, memory in use and
// CPU load, with the switch that starts or stops the engine.
func (s *desktop) engineCard(c *ui.Context, p palette) {
	kind, text := s.engineWords()
	used, total := s.memoryUse()
	ui.Column(c.Key("engine-card")).FillWidth().Shrink(0).Margin(0, 0, 6).Padding(9, 10, 10).Gap(8).
		Radius(8).Background(p.bg).Border(1, p.line).Children(func() {
		ui.Row(c).FillWidth().Gap(8).AlignItems(ui.Center).Children(func() {
			dot(c, p, kind)
			ui.Text(c, text).FontSize(12).FontWeight(600).TextColor(p.text).SingleLine().Grow(1).MinWidth(0)
			s.engineSwitch(c, p)
		})
		if kind != "running" {
			return
		}
		fraction := 0.0
		if total > 0 {
			fraction = float64(used) / float64(total)
		}
		meter(c, p, fraction, 4, p.accent)
		ui.Row(c).FillWidth().Justify(ui.SpaceBetween).Children(func() {
			memory := displaySize(used)
			if total > 0 {
				memory += " / " + displaySize(total)
			}
			ui.Text(c, memory).FontSize(11).FontFeatures("tnum").TextColor(p.muted).SingleLine().Label(tr("内存", "Memory"))
			ui.Textf(c, "CPU %.0f%%", s.cpuNow()).FontSize(11).FontFeatures("tnum").TextColor(p.muted).SingleLine().Shrink(0)
		})
	})
}

// cpuNow is the engine's latest CPU load as a percentage of all its cores.
func (s *desktop) cpuNow() float32 {
	if len(s.cpuHist) == 0 {
		return 0
	}
	return s.cpuHist[len(s.cpuHist)-1]
}

// statusBar is the footer line of engine-wide facts.
func (s *desktop) statusBar(c *ui.Context, p palette) {
	running := 0
	for _, x := range s.containers {
		if x.State == "running" {
			running++
		}
	}
	item := func(text string) { ui.Text(c, text).FontSize(11).FontFeatures("tnum").TextColor(p.muted).SingleLine() }
	ui.Row(c.Key("status")).FillWidth().Height(24).Shrink(0).PaddingX(12).Gap(18).AlignItems(ui.Center).
		Background(p.panel).BorderWidth(1, 0, 0, 0).BorderColor(p.line).Children(func() {
		if v := s.info.ServerVersion; v != "" && v != "0.0.0" {
			item("v" + v)
		}
		item(fmt.Sprintf("%d / %d %s", running, len(s.containers), tr("运行中", "running")))
		if s.contextName != "" {
			item("docker → " + s.contextName)
		}
		ui.Box(c).Grow(1)
		if s.engineState() == "running" {
			item(fmt.Sprintf("CPU %.0f%%", s.cpuNow()))
			if used, total := s.memoryUse(); total > 0 {
				item(displaySize(used) + " / " + displaySize(total))
			}
		}
	})
}

// pageBody shows the current page, or why it cannot be shown.
func (s *desktop) pageBody(c *ui.Context, p palette) {
	for _, n := range navItems {
		if n.page == s.page && n.soon {
			empty(c, p, n.icon, tr(n.zh, n.en)+tr(" · 即将提供", " · coming soon"), "")
			return
		}
	}
	if s.page != settingsPage {
		switch s.engineState() {
		case "offline":
			title := tr("连接不到引擎", "The engine is not answering")
			if s.problem == "" {
				title = tr("正在连接引擎…", "Connecting to the engine…")
			}
			empty(c, p, "power", title, "")
			return
		case "starting":
			empty(c, p, "power", tr("引擎启动中…", "Engine starting…"), "")
			return
		case "stopped":
			if empty(c, p, "power", tr("引擎已停止", "The engine is stopped"), tr("启动引擎", "Start engine")) {
				s.do("engine", engineAction{http.MethodPost, "/kiri/wake", nil}, tr("引擎已启动", "Engine started"))
			}
			return
		}
	}
	switch s.page {
	case overviewPage:
		s.overview(c, p)
	case containersPage:
		s.containersView(c, p)
	case imagesPage:
		s.imagesView(c, p)
	case volumesPage:
		s.volumesView(c, p)
	case networksPage:
		s.networksView(c, p)
	case buildsPage:
		s.buildsView(c, p)
	case settingsPage:
		s.settingsView(c, p)
	}
}

// toolbar is the row above a table: filters on the left, actions on the right.
func toolbar(c *ui.Context, build func()) {
	ui.Row(c.Key("tools")).FillWidth().Height(42).Shrink(0).PaddingX(12).Gap(6).AlignItems(ui.Center).Children(build)
}

// column describes one table column: fixed in width, or sharing spare room.
type column struct {
	title       string
	width, grow float32
	right       bool
}

// tableHeader draws the column titles. actions is the width the rows keep
// for their buttons, so that titles and cells line up.
func tableHeader(c *ui.Context, p palette, cols []column, actions float32) {
	ui.Row(c.Key("head")).FillWidth().Height(26).Shrink(0).Padding(0, 10, 0, 12).Gap(10).AlignItems(ui.Center).
		Background(p.panel).BorderWidth(1, 0, 1, 0).BorderColor(p.line).Children(func() {
		for _, col := range cols {
			cell(c, col.width, col.grow).Children(func() {
				t := ui.Text(c, col.title).FontSize(10.5).LetterSpacing(0.5).TextColor(p.muted).SingleLine()
				if col.right {
					t.FillWidth().TextAlign(ui.End)
				}
			})
		}
		ui.Box(c).Width(actions).Shrink(0)
	})
}

// tableRow is one selectable row. The cells sit inside a single button that
// selects the row; the action buttons stay outside it, so they are never
// nested in another button.
func (s *desktop) tableRow(c *ui.Context, p palette, id, name string, actionsWidth float32, cells, actions func()) {
	on := s.selected[s.page] == id
	row := ui.Row(c).FillWidth().Height(s.rowHeight()).Shrink(0).AlignItems(ui.Center).
		BorderWidth(0, 0, 1, 0).BorderColor(p.line)
	bar := ui.Transparent
	if on {
		row.Background(p.raised)
		bar = p.accent
	} else if row.Hovered() {
		row.Background(p.hover)
	}
	row.Children(func() {
		ui.Box(c).Width(2).FillHeight().Shrink(0).Background(bar)
		b := clickable(c.Key("open")).Label(tr("查看 ", "Details for ")+name).Grow(1).Width(0).MinWidth(0).FillHeight().
			Padding(0, 0, 0, 10).Gap(10).AlignItems(ui.Center).Justify(ui.Start)
		b.Children(cells)
		if b.Clicked() {
			s.selectRow(id)
		}
		ui.Row(c.Key("actions")).Width(actionsWidth).Shrink(0).Margin(0, 10, 0, 10).Gap(1).Justify(ui.End).AlignItems(ui.Center).Children(actions)
	})
	s.visible = append(s.visible, id)
}

// dock is the detail panel pinned under a table. Dragging its top edge
// resizes it; body builds the chosen tab.
func (s *desktop) dock(c *ui.Context, p palette, title string, sub func(), tabs []string, actions func(), body func(tab int)) {
	tab := min(max(0, s.tab[s.page]), len(tabs)-1)
	panel := ui.Column(c.Key("dock")).FillWidth().Background(p.panel)
	if s.dockMax {
		panel.Grow(1).MinHeight(0)
	} else {
		panel.Height(s.prefs.DockHeight).Shrink(0)
	}
	panel.Children(func() {
		grip := ui.Box(c.Key("dock-grip")).FillWidth().Height(4).Shrink(0).Cursor(ui.CursorResizeRow).
			BorderWidth(1, 0, 0, 0).BorderColor(p.line).Label(tr("调整高度", "Resize"))
		if grip.Hovered() || grip.Pressed() {
			grip.Background(p.accent.Alpha(0.5))
		}
		// Dragged reports the movement since the last frame; dragging up
		// makes the dock taller.
		if _, dy, held := grip.Dragged(); held && dy != 0 {
			s.prefs.DockHeight = min(520, max(140, s.prefs.DockHeight-dy))
		}
		if _, _, ended := grip.DragEnded(); ended {
			s.savePrefs()
		}
		ui.Row(c.Key("dock-bar")).FillWidth().Height(34).Shrink(0).Padding(0, 8, 0, 12).Gap(6).AlignItems(ui.Center).
			BorderWidth(0, 0, 1, 0).BorderColor(p.line).Children(func() {
			ui.Text(c, title).Font(mono).FontSize(12).Bold().TextColor(p.text).SingleLine().MaxWidth(280).MinWidth(0)
			if sub != nil {
				sub()
			}
			ui.Row(c.Key("tabs")).Shrink(0).Margin(0, 0, 0, 8).FillHeight().Children(func() {
				for i, name := range tabs {
					b := clickable(c.Key(i)).Label(name).FillHeight().PaddingX(10).Center()
					ink, weight := p.muted, 400
					if i == tab {
						ink, weight = p.text, 600
						b.BorderWidth(0, 0, 2, 0).BorderColor(p.accent)
					} else if b.Hovered() {
						ink = p.text
					}
					b.Children(func() { ui.Text(c, name).FontSize(12).FontWeight(weight).TextColor(ink) })
					if b.Clicked() {
						s.tab[s.page] = i
						s.refreshing = false
						s.refresh()
					}
				}
			})
			ui.Box(c).Grow(1)
			if actions != nil {
				actions()
				ui.Box(c).Width(1).Height(14).Shrink(0).Margin(0, 4).Background(p.line)
			}
			if s.dockMax {
				if iconButton(c.Key("dock-restore"), p, "collapse", tr("还原", "Restore"), false).Clicked() {
					s.dockMax = false
				}
			} else if iconButton(c.Key("dock-max"), p, "expand", tr("最大化", "Maximize"), false).Clicked() {
				s.dockMax = true
			}
			if iconButton(c.Key("dock-close"), p, "x", tr("关闭", "Close"), false).Clicked() {
				s.selectRow("")
			}
		})
		ui.Column(c.Key("dock-body-" + strconv.Itoa(tab))).FillWidth().Grow(1).MinHeight(0).Children(func() { body(tab) })
	})
}

// keyValues is the body of an info tab.
func keyValues(c *ui.Context, p palette, fields []field) {
	ui.Scroll(c.Key("kv")).Fill().Grow(1).MinHeight(0).Children(func() {
		ui.Column(c).FillWidth().Padding(10, 14).Gap(7).Children(func() {
			for i, f := range fields {
				ui.Row(c.Key(i)).FillWidth().Gap(16).AlignItems(ui.Start).Children(func() {
					ui.Text(c, f.label).Width(120).Shrink(0).FontSize(11.5).TextColor(p.muted)
					ui.Text(c, f.value).Width(0).Grow(1).MinWidth(0).Font(mono).FontSize(11.5).TextColor(p.text).Selectable()
				})
			}
		})
	})
}

// note is a centered line of muted text, for an empty tab.
func note(c *ui.Context, p palette, text string) {
	ui.Row(c).FillWidth().Padding(22, 14).Justify(ui.Center).Children(func() {
		ui.Text(c, text).FontSize(12).TextColor(p.muted)
	})
}

// users lists the containers attached to a volume or network; a click opens
// the container.
func (s *desktop) users(c *ui.Context, p palette, list []container) {
	if len(list) == 0 {
		note(c, p, tr("没有容器在使用", "No containers use it"))
		return
	}
	ui.Scroll(c.Key("users")).Fill().Grow(1).MinHeight(0).Children(func() {
		for _, x := range list {
			b := clickable(c.Key(x.ID)).Label(tr("打开 ", "Open ")+x.Name).FillWidth().Height(26).PaddingX(14).Gap(10).
				AlignItems(ui.Center).Justify(ui.Start).BorderWidth(0, 0, 1, 0).BorderColor(p.line)
			if b.Hovered() {
				b.Background(p.hover)
			}
			b.Children(func() {
				dot(c, p, x.bucket())
				cell(c, 0, 1).Children(func() { label(c, x.Name, p.text) })
				cell(c, 0, 1).Children(func() { label(c, x.Image, p.muted) })
				cell(c, 170, 0).Children(func() { label(c, x.status(s.now), stateColor(p, x.bucket())) })
			})
			if b.Clicked() {
				s.open(containersPage, x.ID)
			}
		}
	})
}

// confirm shows the pending question. Cancel and Escape drop it.
func (s *desktop) confirm(c *ui.Context, p palette) {
	open := s.pending != nil
	ui.DialogBase(c.Key("confirmation"), &open, func(backdrop, dialog ui.Element) {
		q := s.pending
		backdrop.Background(ui.RGBA(0, 0, 0, 0.45))
		dialog.Width(380).Padding(16).Gap(12).Radius(8).Background(p.panel).Border(1, p.line).
			Shadow(0, 24, 60, -10, ui.RGBA(0, 0, 0, 0.5))
		ui.Text(c, q.title).FontSize(13.5).FontWeight(600).TextColor(p.text)
		if q.body != "" {
			ui.Text(c, q.body).FontSize(12).LineHeight(1.5).TextColor(p.muted)
		}
		ui.Row(c).FillWidth().Gap(8).Justify(ui.End).Padding(4, 0, 0).Children(func() {
			if button(c.Key("cancel"), p, tr("取消", "Cancel"), "", plainButton, false) {
				open = false
			}
			if open && button(c.Key("confirm"), p, q.ok, "", dangerButton, false) {
				open = false
				q.run()
			}
		})
	})
	if !open {
		s.pending = nil
	}
}

// input is one labelled field of a dialog.
func input(c *ui.Context, p palette, key, title, hint string, value *string, focus bool) ui.Element {
	var field ui.Element
	ui.Column(c.Key("field-" + key)).FillWidth().Gap(5).Children(func() {
		ui.Text(c, title).FontSize(11.5).TextColor(p.muted)
		field = ui.TextInput(c.Key(key), value).Label(title).Placeholder(hint).FillWidth().Height(30).Font(mono).FontSize(12)
		if focus {
			field.AutoFocus()
		}
	})
	return field
}

// dialog shows the open form: run a container, pull an image, or name a new
// volume or network.
func (s *desktop) dialog(c *ui.Context, p palette) {
	open := s.form.kind != ""
	ui.DialogBase(c.Key("form-"+s.form.kind), &open, func(backdrop, dialog ui.Element) {
		f := &s.form
		backdrop.Background(ui.RGBA(0, 0, 0, 0.45))
		dialog.Width(440).Padding(16).Gap(12).Radius(8).Background(p.panel).Border(1, p.line).
			Shadow(0, 24, 60, -10, ui.RGBA(0, 0, 0, 0.5))
		var title, ok string
		submitted := false
		switch f.kind {
		case "run":
			title, ok = tr("运行容器", "Run a container"), tr("运行", "Run")
			ui.Text(c, title).FontSize(13.5).FontWeight(600).TextColor(p.text)
			submitted = input(c, p, "image", tr("镜像", "Image"), "nginx:alpine", &f.image, !f.named).Submitted()
			submitted = input(c, p, "name", tr("名称", "Name"), tr("自动生成", "Generated"), &f.name, f.named).Submitted() || submitted
			ui.Row(c).FillWidth().Gap(10).AlignItems(ui.End).Children(func() {
				submitted = input(c, p, "host", tr("主机端口", "Host port"), "8080", &f.host, false).Submitted() || submitted
				submitted = input(c, p, "port", tr("容器端口", "Container port"), "80", &f.port, false).Submitted() || submitted
			})
			submitted = input(c, p, "command", tr("命令", "Command"), tr("使用镜像默认值", "Image default"), &f.value, false).Submitted() || submitted
		case "pull":
			title, ok = tr("拉取镜像", "Pull an image"), tr("拉取", "Pull")
			ui.Text(c, title).FontSize(13.5).FontWeight(600).TextColor(p.text)
			submitted = input(c, p, "image", tr("镜像", "Image"), "ubuntu:24.04", &f.image, true).Submitted()
		default:
			title, ok = tr("创建卷", "Create a volume"), tr("创建", "Create")
			if f.kind == "network" {
				title = tr("创建网络", "Create a network")
			}
			ui.Text(c, title).FontSize(13.5).FontWeight(600).TextColor(p.text)
			submitted = input(c, p, "name", tr("名称", "Name"), "my_"+f.kind, &f.name, true).Submitted()
		}
		if f.problem != "" {
			ui.Text(c, f.problem).FontSize(12).TextColor(p.err)
		}
		ui.Row(c).FillWidth().Gap(8).Justify(ui.End).Padding(4, 0, 0).Children(func() {
			if button(c.Key("cancel"), p, tr("取消", "Cancel"), "", plainButton, false) {
				open = false
			}
			if button(c.Key("submit"), p, ok, "", primaryButton, false) {
				submitted = true
			}
		})
		if submitted && open && s.submit() {
			open = false
		}
	})
	if !open {
		s.form = form{}
	}
}

// submit checks the open form and sends it. It reports whether the dialog
// can close; a form with a problem stays open and says what is wrong.
func (s *desktop) submit() bool {
	f := &s.form
	f.image, f.name = strings.TrimSpace(f.image), strings.TrimSpace(f.name)
	switch f.kind {
	case "run":
		spec := runSpec{Image: f.image, Name: f.name, Command: f.value}
		if spec.Image == "" {
			f.problem = tr("请填写镜像", "Enter an image")
			return false
		}
		if f.host != "" || f.port != "" {
			var e1, e2 error
			spec.HostPort, e1 = strconv.Atoi(strings.TrimSpace(f.host))
			spec.ContainerPort, e2 = strconv.Atoi(strings.TrimSpace(f.port))
			if e1 != nil || e2 != nil || spec.HostPort < 1 || spec.HostPort > 65535 || spec.ContainerPort < 1 || spec.ContainerPort > 65535 {
				f.problem = tr("两个端口都要填 1–65535 的数字", "Both ports must be numbers from 1 to 65535")
				return false
			}
		}
		for _, x := range s.containers {
			if spec.Name != "" && x.Name == spec.Name {
				f.problem = tr("已有同名容器", "A container with this name exists")
				return false
			}
		}
		s.run(2*time.Minute, func(ctx context.Context) func() {
			err := s.engine.run(ctx, spec)
			return func() { s.settle(err, tr("已启动 ", "Started ")+spec.Image) }
		})
		s.page = containersPage
	case "pull":
		ref := f.image
		if ref == "" {
			f.problem = tr("请填写镜像", "Enter an image")
			return false
		}
		s.working["pull"] = true
		s.run(10*time.Minute, func(ctx context.Context) func() {
			err := s.engine.pull(ctx, ref)
			return func() {
				delete(s.working, "pull")
				s.settle(err, tr("已拉取 ", "Pulled ")+ref)
			}
		})
		if s.engine.http == nil {
			delete(s.working, "pull")
		}
	default:
		name := f.name
		if name == "" {
			f.problem = tr("请填写名称", "Enter a name")
			return false
		}
		if f.kind == "network" {
			s.do("create", engineAction{http.MethodPost, "/networks/create", map[string]any{"Name": name, "Driver": "bridge"}}, tr("已创建 ", "Created ")+name)
		} else {
			s.do("create", engineAction{http.MethodPost, "/volumes/create", map[string]any{"Name": name}}, tr("已创建 ", "Created ")+name)
		}
	}
	return true
}
