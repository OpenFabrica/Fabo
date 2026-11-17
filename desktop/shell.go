package main

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/egoist/mygo/ui"
)

const (
	stripHeight      = 40
	sidebarOpenWidth = 212
	sidebarIconWidth = 56
	// The detail panel beside a list is dragged between these widths.
	dockMinWidth = 320
	dockMaxWidth = 900
	// pageInset is the left and right margin of everything on the page card.
	pageInset = 24
	// A window narrower than autoFoldWidth folds the sidebar. A table
	// narrower than narrowListWidth drops its optional columns, and one
	// narrower than tightListWidth its state columns too. The widths are
	// what the container table's columns need: inset, checkbox, a 150 wide
	// name, the columns kept and the action buttons, with the gaps between.
	autoFoldWidth   = 1040
	narrowListWidth = 900 // 24 + 150 + 504 columns + 72 gaps + 148
	tightListWidth  = 560 // 24 + 150 + 150 + 52 + 24 gaps + 148
)

// rowHeight is the height of a table row at the chosen density. Container
// rows are taller: they show a tile and the image under the name.
func (s *desktop) rowHeight() float32 {
	h := float32(40)
	if s.prefs.Density == "cozy" {
		h = 48
	}
	if s.page == containersPage {
		h += 14
	}
	return h
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
	if s.shares.billingURL != "" {
		c.OpenURL(s.shares.billingURL)
		s.shares.billingURL = ""
	}
	lang = s.prefs.language(s.locale)
	mono = "monospace"
	if s.prefs.Mono != "" {
		mono = s.prefs.Mono
	}
	c.SetZoom(s.prefs.Scale)
	// Layout works in zoomed units, so the window's width is measured in them.
	if w, _ := c.Size(); w > 0 {
		s.width = w / max(s.prefs.Scale, 0.5)
	}
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

	if s.host != nil && s.win.status.SetupPending {
		// First run on Windows: setup is the whole window until it is done.
		s.setupPage(c, p)
		for _, notice := range s.notices {
			c.Toast(notice)
		}
		s.notices = nil
		return
	}
	s.reapClosedSessions()
	s.syncLogStreams()
	s.importWatch()
	s.visible = s.visible[:0]
	// The window is a grey shell: the title strip and the sidebar sit on
	// it, and the page is a white card beside them. A page that shows a
	// detail panel registers it while it builds; the panel is placed to the
	// right of the page once the page is done.
	s.detailPanel = nil
	ui.Column(c).Fill().Background(p.bg).Children(func() {
		s.strip(c, p)
		ui.Row(c.Key("body")).FillWidth().Grow(1).MinHeight(0).AlignItems(ui.Stretch).Children(func() {
			s.sidebar(c, p)
			ui.Column(c.Key("card")).Grow(1).Width(0).MinWidth(0).Margin(0, 8, 8, 0).Radius(14).
				Background(p.panel).Border(1, p.line).Shadow(0, 1, 3, 0, ui.RGBA(16, 24, 40, 0.04)).Clip().Children(func() {
				s.pageHead(c, p)
				ui.Row(c.Key("page-" + s.page)).FillWidth().Grow(1).MinHeight(0).AlignItems(ui.Stretch).Children(func() {
					main := ui.Column(c.Key("main")).Grow(1).Width(0).MinWidth(0)
					main.Children(func() { s.pageBody(c, p) })
					if panel := s.detailPanel; panel != nil {
						// A maximized panel takes the whole card; the list
						// keeps its state while it is hidden.
						main.Hidden(s.dockMax)
						panel()
					}
				})
				// One strip at a time: an engine to restart comes first.
				if !s.engineStrip(c, p) && !s.updateStrip(c, p) {
					s.importStrip(c, p)
				}
				s.switchCardView(c, p)
			})
		})
	})
	s.confirm(c, p)
	s.dialog(c, p)
	s.buildAsk(c, p)
	s.reviewWindow(c, p)
	s.switchDialog(c, p)
	s.announceShares(c)
	for _, notice := range s.notices {
		c.Toast(notice)
	}
	s.notices = nil
	s.keys(c)
	s.syncLogStreams()
}

// keys handles the window's shortcuts: ⌘K to search, ⌘B to fold the sidebar,
// and the arrows and Escape for the selection of the list on screen.
func (s *desktop) keys(c *ui.Context) {
	c.OnWindowShortcut(ui.Cmd, ui.KeyK, func() { s.searchBox.Focus() })
	c.OnWindowShortcut(ui.Cmd, ui.KeyB, s.toggleSidebar)
	if s.pending != nil || s.form.kind != "" || c.FocusTakesText() || s.page == terminalPage || (s.page == containersPage && s.tab[containersPage] == 2 && s.selected[containersPage] != "") {
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

// navFolded reports whether the sidebar shows icons only. A window
// narrower than autoFoldWidth folds it whatever the saved choice is, unless
// the user unfolded it for this narrow spell.
func (s *desktop) navFolded() bool {
	if s.width > 0 && s.width < autoFoldWidth {
		return !s.navPeek
	}
	return s.prefs.NavCollapsed
}

// toggleSidebar folds or unfolds the sidebar. In a narrow window that only
// lasts until the window widens again; otherwise the choice is saved.
func (s *desktop) toggleSidebar() {
	if s.width > 0 && s.width < autoFoldWidth {
		s.navPeek = !s.navPeek
		return
	}
	s.navPeek = false
	s.prefs.NavCollapsed = !s.prefs.NavCollapsed
	s.savePrefs()
}

// searchable reports whether the page has a list the title bar's field filters.
func searchable(page string) bool {
	return page == containersPage || page == imagesPage || page == volumesPage || page == networksPage || page == kubernetesPage || page == machinesPage || page == usbPage
}

// strip is the window's title bar, on the shell's grey: the traffic lights,
// the sidebar switch and the mark, and the engine's state while the sidebar
// is folded. It carries no border; the page card below separates itself.
func (s *desktop) strip(c *ui.Context, p palette) {
	left := max(14, c.TitleBar().Left+6)
	ui.Row(c.Key("strip")).FillWidth().Height(stripHeight).Shrink(0).Padding(0, 14, 0, left).Gap(10).AlignItems(ui.Center).
		DragWindow().Children(func() {
		toggle := tr("收起侧栏", "Hide sidebar")
		if s.navFolded() {
			toggle = tr("展开侧栏", "Show sidebar")
		}
		if iconButton(c.Key("sidebar-toggle"), p, "sidebar", toggle, false).Clicked() {
			s.toggleSidebar()
		}
		ui.Image(c, markIcon).Size(20, 20).Shrink(0)
		ui.Text(c, "Fabo").FontSize(14).FontWeight(600).LetterSpacing(-0.1).TextColor(p.text).Shrink(0)
		ui.Box(c).Grow(1)
		if s.navFolded() {
			s.engineLine(c, p)
		}
		if searchable(s.page) {
			s.searchField(c, p)
		}
		for _, n := range []navItem{{terminalPage, "terminal", "终端", "Terminal", false}, {settingsPage, "settings", "设置", "Settings", false}} {
			b := iconButton(c.Key("strip-"+n.page), p, n.icon, tr(n.zh, n.en), false)
			if s.page == n.page {
				b.Background(p.raised)
			}
			if b.Clicked() {
				s.navigate(n.page)
			}
		}
	})
}

// hintTitle is a page title inside the search hint: lowercased, unless it
// starts with an acronym such as USB.
func hintTitle(title string) string {
	if len(title) >= 2 && title[0] >= 'A' && title[0] <= 'Z' && title[1] >= 'A' && title[1] <= 'Z' {
		return title
	}
	return strings.ToLower(title)
}

// searchField filters the current page's list from the title strip. It
// shows ⌘K, the shortcut that focuses it, until something is typed and the
// field's own clear button takes that place.
func (s *desktop) searchField(c *ui.Context, p palette) {
	ui.Box(c.Key("search-box")).Width(340).Shrink(1).MinWidth(160).Height(30).Children(func() {
		hint := tr("搜索", "Search ") + hintTitle(pageTitle(s.page)) + "…"
		field := ui.SearchField(c.Key("search-"+s.page), &s.search).Placeholder(hint).Label(tr("搜索", "Search")).
			Fill().Radius(8).Background(p.panel).Border(1, p.line).Bind(&s.searchBox)
		if field.Changed() {
			s.list(s.page).ScrollTo(0, ui.Start)
		}
		if s.search == "" {
			ui.Box(c.Key("search-keys")).Absolute().Right(6).Top(5).Height(20).PaddingX(6).Radius(5).Background(p.soft).Center().Children(func() {
				ui.Text(c, "⌘K").FontSize(11).FontWeight(500).TextColor(p.muted)
			})
		}
	})
}

// pageSummary is the short fact beside a page's title.
func (s *desktop) pageSummary() string {
	switch s.page {
	case containersPage:
		running := 0
		for _, x := range s.containers {
			if x.State == "running" {
				running++
			}
		}
		return fmt.Sprintf(tr("%d 个运行中", "%d running"), running)
	case machinesPage:
		running := 0
		for _, x := range s.machines {
			if x.State == "running" {
				running++
			}
		}
		return fmt.Sprintf(tr("%d 个运行中", "%d running"), running)
	case kubernetesPage:
		if s.kube.tab == 1 {
			return "Services"
		}
		return "Pods"
	case imagesPage:
		var total int64
		for _, img := range s.images {
			total += img.Size
		}
		return displaySize(total)
	case usbPage:
		if n := s.usbInFabo(); n > 0 {
			return fmt.Sprintf(tr("%d 个在 Fabo 中", "%d in Fabo"), n)
		}
		return ""
	case sharePage:
		if !s.shares.ok || !s.shares.info.Account.SignedIn {
			return ""
		}
		live := 0
		for _, l := range s.shares.info.Shares {
			if l.State == "live" {
				live++
			}
		}
		return fmt.Sprintf(tr("%d 个在线", "%d live"), live)
	}
	return ""
}

// pageHead is the top of the page card: the page's title with its summary.
func (s *desktop) pageHead(c *ui.Context, p palette) {
	ui.Row(c.Key("page-head")).FillWidth().Shrink(0).Padding(20, pageInset, 14, pageInset).Gap(14).AlignItems(ui.Center).Children(func() {
		// A machine's terminal page is titled with the machine, with a way
		// back to the list that leaves its shells open.
		if s.page == machinesPage && s.machineTerm != "" {
			if iconButton(c.Key("machines-back"), p, "back", tr("返回 Linux 机器", "Back to machines"), false).Clicked() {
				s.machineTerm = ""
			}
			ui.Text(c, s.machineTerm).FontSize(21).FontWeight(600).LetterSpacing(-0.3).TextColor(p.text).Shrink(0)
			for _, m := range s.machines {
				if m.Machine == s.machineTerm {
					ui.Text(c, distroName(m.Image)).FontSize(13).TextColor(p.muted).SingleLine().MinWidth(0)
				}
			}
			return
		}
		ui.Text(c, pageTitle(s.page)).FontSize(21).FontWeight(600).LetterSpacing(-0.3).TextColor(p.text).Shrink(0)
		if sum := s.pageSummary(); sum != "" && s.engineState() == "running" {
			ui.Text(c, sum).FontSize(13).FontFeatures("tnum").TextColor(p.muted).SingleLine().MinWidth(0)
		}
	})
}

// vmName names what runs the containers, as Kiri does: a Linux VM on the
// Mac, or the engine itself where it runs directly on Linux.
func (s *desktop) vmName() string {
	if s.info.FaboVMControls != nil && !*s.info.FaboVMControls {
		return tr("引擎", "Engine")
	}
	return tr("Linux 虚拟机", "Linux VM")
}

// engineWords gives the engine's state as a dot kind and a word, in the
// words Settings uses.
func (s *desktop) engineWords() (kind, text string) {
	switch s.engineState() {
	case "running":
		return "running", tr("运行中", "Running")
	case "starting":
		return "busy", tr("启动中…", "Starting…")
	case "stopping":
		return "busy", tr("停止中…", "Stopping…")
	case "asleep":
		return "stopped", tr("休眠中", "Asleep")
	case "stopped":
		return "stopped", tr("已关闭", "Off")
	}
	return "failed", tr("未连接", "Not connected")
}

// engineSwitch is the button that starts or stops the engine. An engine that
// runs directly on Linux has no VM to control and shows none.
func (s *desktop) engineSwitch(c *ui.Context, p palette) {
	if s.info.FaboVMControls != nil && !*s.info.FaboVMControls {
		return
	}
	switch s.engineState() {
	case "running":
		if iconButton(c.Key("engine-stop"), p, "power", tr("停止引擎", "Stop engine"), false).Clicked() {
			s.stopEngine()
		}
	case "stopped", "asleep":
		if iconButton(c.Key("engine-start"), p, "play", tr("启动引擎", "Start engine"), false).Clicked() {
			s.do("engine", engineAction{http.MethodPost, "/fabo/wake", nil}, tr("引擎已启动", "Engine started"))
		}
	}
}

// engineLine is the one-line engine state of the title bar.
func (s *desktop) engineLine(c *ui.Context, p palette) {
	kind, text := s.engineWords()
	text = s.vmName() + " · " + text
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

// sidebar is the navigation, drawn straight on the shell. Open, it names
// each page with its count and carries the engine card; folded, it shows
// icons only. Pages that are not built yet stay out of it.
func (s *desktop) sidebar(c *ui.Context, p palette) {
	open := !s.navFolded()
	width := float32(sidebarIconWidth)
	if open {
		width = sidebarOpenWidth
	}
	counts := map[string]int{containersPage: len(s.containers), imagesPage: len(s.images), volumesPage: len(s.volumes), networksPage: len(s.networks), machinesPage: len(s.machines)}
	side := ui.Column(c.Key("sidebar")).Width(width).Shrink(0).Padding(6, 12, 12).Gap(2).Justify(ui.Start)
	if !open {
		side.Padding(6, 10, 12).AlignItems(ui.Center)
	}
	side.Children(func() {
		for _, n := range navItems {
			// Terminal and Settings live in the title strip.
			if s.navSoon(n) || n.page == terminalPage || n.page == settingsPage {
				continue
			}
			if n.page == kubernetesPage {
				s.kubernetesGroup(c, p, open)
				continue
			}
			count, counted := counts[n.page]
			if s.navButton(c.Key("nav-"+n.page), p, n, open, s.page == n.page, count, counted) {
				s.navigate(n.page)
			}
		}
		ui.Box(c).Grow(1)
		if open {
			s.engineCard(c, p)
		}
	})
}

// clusterRunning reports whether the Kubernetes node runs. The container
// list is read on every refresh, so the sidebar knows without asking the
// cluster; the cluster's own reading stands in before the list arrives.
func (s *desktop) clusterRunning() bool {
	for _, x := range s.containers {
		if x.Name == kubernetesNode {
			return x.State == "running"
		}
	}
	return s.kube.state == "running"
}

// kubernetesGroup is the sidebar's Kubernetes section: a heading with the
// cluster's settings and state, then Pods and Services, each opening the
// Kubernetes page on its list. Folded, a rule stands for the heading.
func (s *desktop) kubernetesGroup(c *ui.Context, p palette, open bool) {
	if open {
		ui.Row(c.Key("k8s-head")).FillWidth().Height(32).Shrink(0).Margin(14, 0, 2).Padding(0, 2, 0, 10).Gap(6).AlignItems(ui.Center).Children(func() {
			ui.Text(c, "KUBERNETES").FontSize(11).FontWeight(600).LetterSpacing(0.8).TextColor(p.muted).SingleLine().Grow(1).MinWidth(0)
			if iconButton(c.Key("k8s-settings"), p, "wrench", tr("Kubernetes 设置", "Kubernetes settings"), false).Clicked() {
				s.navigate(settingsPage)
				for i, t := range settingsTabs {
					if t.id == "kubernetes" {
						s.settingsTab = i
					}
				}
			}
			kind, word := "stopped", tr("已关闭", "Off")
			switch {
			case s.working["k8s"]:
				kind, word = "busy", tr("配置中", "Configuring")
			case s.clusterRunning():
				kind, word = "running", tr("运行中", "Running")
			}
			ui.Row(c).Shrink(0).Gap(6).AlignItems(ui.Center).Padding(0, 6, 0, 0).Children(func() {
				dot(c, p, kind)
				ui.Text(c, word).FontSize(12).TextColor(p.muted)
			})
		})
	} else {
		ui.Box(c).Width(24).Height(1).Shrink(0).Margin(8, 0).Background(p.line)
	}
	for i, kind := range []struct{ icon, name string }{{"hexagon", "Pods"}, {"plug", "Services"}} {
		n := navItem{page: kubernetesPage, icon: kind.icon, zh: kind.name, en: kind.name}
		if s.navButton(c.Key("nav-k8s-"+kind.name), p, n, open, s.page == kubernetesPage && s.kube.tab == i, 0, false) {
			s.navigate(kubernetesPage)
			s.kube.tab = i
		}
	}
}

// navButton is one sidebar entry; on marks the one showing. The current
// entry is a small white card lifted off the shell, its icon in the accent.
func (s *desktop) navButton(c *ui.Context, p palette, n navItem, open, on bool, count int, counted bool) bool {
	name := tr(n.zh, n.en)
	b := clickable(c).Label(name).Radius(9).Shrink(0).AlignItems(ui.Center)
	if open {
		b.FillWidth().Height(36).PaddingX(10).Gap(10).Justify(ui.Start)
	} else {
		b.Size(36, 36).Justify(ui.Center).Tooltip(name)
	}
	ink, text := p.sub, p.sub
	switch {
	case on:
		b.Background(p.panel).Border(1, p.line).Shadow(0, 1, 2, 0, ui.RGBA(16, 24, 40, 0.05))
		ink, text = p.accent, p.text
	case b.Hovered():
		b.Background(p.raised)
		ink, text = p.text, p.text
	}
	b.Children(func() {
		ui.Icon(c, icons[n.icon]).Size(18, 18).Shrink(0).TextColor(ink)
		if !open {
			return
		}
		weight := 400
		if on {
			weight = 500
		}
		ui.Text(c, name).FontSize(13).FontWeight(weight).TextColor(text).SingleLine().Grow(1).MinWidth(0)
		if counted {
			ui.Textf(c, "%d", count).FontSize(12).FontFeatures("tnum").TextColor(p.muted)
		}
	})
	return b.Clicked()
}

// engineCard sits at the foot of the open sidebar: state and version, the
// switch that starts or stops the engine, and the memory in use. The version
// is a line of its own whenever the engine answers, not only a tooltip.
func (s *desktop) engineCard(c *ui.Context, p palette) {
	state := s.engineState()
	kind, text := s.engineWords()
	used, total := s.memoryUse()
	version := ""
	if v := s.info.ServerVersion; v != "" && v != "0.0.0" {
		version = "v" + v
	}
	ui.Column(c.Key("engine-card")).FillWidth().Shrink(0).Margin(0, 0, 8).Padding(12).Gap(10).
		Radius(12).Background(p.panel).Border(1, p.line).Children(func() {
		head := ui.Row(c.Key("engine-head")).FillWidth().Gap(7).AlignItems(ui.Center)
		if version != "" {
			head.Tooltip(s.vmName() + " · " + version)
		}
		head.Children(func() {
			dot(c, p, kind)
			ui.Text(c, s.vmName()).FontSize(12.5).FontWeight(600).TextColor(p.text).SingleLine().Grow(1).MinWidth(0)
			ui.Text(c, text).FontSize(12).TextColor(p.muted).SingleLine().Shrink(0)
		})
		// The engine's version. A development build's is long
		// (0.5.0-3-g1a2b3c4-dirty): it is cut short, not the label.
		versionRow := func() {
			row := ui.Row(c.Key("engine-version")).FillWidth().Gap(10)
			ink := p.text
			// An engine of another version than the window waits for a
			// restart: say so here too once its strip is put off.
			if _, stale := s.staleEngine(); stale {
				ink = p.warn
				row.Tooltip(fmt.Sprintf(tr("已安装 %s %s，重启引擎后生效", "%s %s is installed; restart the engine to run it"), s.appName(), release))
			}
			row.Children(func() {
				ui.Text(c, tr("版本", "Version")).FontSize(12).TextColor(p.muted).SingleLine().Shrink(0)
				ui.Text(c, version).FontSize(12).FontFeatures("tnum").TextColor(ink).SingleLine().Grow(1).MinWidth(0).TextAlign(ui.End)
			})
		}
		// What the VM holds, while it is up: containers, CPU and memory.
		if state == "running" || state == "asleep" {
			ui.Column(c).FillWidth().Gap(4).Children(func() {
				fact := func(name, value string) {
					ui.Row(c).FillWidth().Gap(10).Children(func() {
						ui.Text(c, name).FontSize(12).TextColor(p.muted).Grow(1).MinWidth(0).SingleLine()
						ui.Text(c, value).FontSize(12).FontFeatures("tnum").TextColor(p.text).SingleLine().Shrink(0)
					})
				}
				if version != "" {
					versionRow()
				}
				fact(tr("运行中", "Running"), strconv.Itoa(s.info.ContainersRunning))
				cpus := strconv.Itoa(s.cores())
				if state == "running" && len(s.cpuHist) > 0 {
					cpus = fmt.Sprintf("%.0f%% · %s", s.cpuNow(), cpus)
				}
				fact(tr("CPU", "CPUs"), cpus)
				memory := displaySize(used)
				if total > 0 {
					memory += " / " + displaySize(total)
				}
				fact(tr("内存", "Memory"), memory)
				fraction := 0.0
				if total > 0 {
					fraction = float64(used) / float64(total)
				}
				ui.Box(c).FillWidth().Margin(2, 0, 0).Children(func() { meter(c, p, fraction, 4, p.accent) })
			})
		} else if version != "" && state != "offline" {
			// A stopped VM's engine still answers with its version.
			versionRow()
		}
		// An engine on Linux itself has no VM to switch; nor can an
		// unreachable one be switched from here.
		if s.info.FaboVMControls != nil && !*s.info.FaboVMControls || state == "offline" {
			return
		}
		ui.Column(c).FillWidth().Children(func() {
			switch state {
			case "running", "asleep":
				if labeledButton(c.Key("engine-toggle"), p, tr("停止虚拟机", "Stop VM"), tr("停止引擎", "Stop engine"), "", plainButton, false) {
					s.stopEngine()
				}
			case "stopped":
				if labeledButton(c.Key("engine-toggle"), p, tr("启动虚拟机", "Start VM"), tr("启动引擎", "Start engine"), "", primaryButton, false) {
					s.do("engine", engineAction{http.MethodPost, "/fabo/wake", nil}, tr("引擎已启动", "Engine started"))
				}
			default:
				labeledButton(c.Key("engine-toggle"), p, text, text, "", plainButton, true)
			}
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

// pageBody shows the current page, or why it cannot be shown.
func (s *desktop) pageBody(c *ui.Context, p palette) {
	for _, n := range navItems {
		if n.page == s.page && s.navSoon(n) {
			empty(c, p, n.icon, tr(n.zh, n.en)+tr(" · 即将提供", " · coming soon"), "")
			return
		}
	}
	// The USB page needs fabo-win only: a device left in WSL can be given
	// back while the engine is stopped.
	if s.page != settingsPage && s.page != terminalPage && !(s.page == usbPage && s.hasUSB()) {
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
		case "stopping":
			empty(c, p, "power", tr("引擎停止中…", "Engine stopping…"), "")
			return
		case "stopped":
			if empty(c, p, "power", tr("引擎已停止", "The engine is stopped"), tr("启动引擎", "Start engine")) {
				s.do("engine", engineAction{http.MethodPost, "/fabo/wake", nil}, tr("引擎已启动", "Engine started"))
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
	case machinesPage:
		s.machinesView(c, p)
	case terminalPage:
		s.hostTerminalView(c, p)
	case kubernetesPage:
		s.kubernetesView(c, p)
	case sharePage:
		s.shareView(c, p)
	case usbPage:
		s.usbView(c, p)
	case settingsPage:
		s.settingsView(c, p)
	}
}

// toolbar is the row above a table: filters on the left, actions on the right.
func toolbar(c *ui.Context, build func()) {
	ui.Row(c.Key("tools")).FillWidth().Shrink(0).Padding(0, pageInset, 12, pageInset).Gap(8).AlignItems(ui.Center).Children(build)
}

// column describes one table column: fixed in width, or sharing spare room.
type column struct {
	title       string
	width, grow float32
	right       bool
}

// dockWidth is the detail panel's width: the saved one, but at most half
// of the page card, so a narrow window keeps room for its list.
func (s *desktop) dockWidth() float32 {
	if s.width <= 0 {
		return s.prefs.DockWidth
	}
	return min(s.prefs.DockWidth, max(280, s.cardWidth()/2))
}

// cardWidth is about how wide the page card is: the window less the
// sidebar and the card's margin.
func (s *desktop) cardWidth() float32 {
	side := float32(sidebarOpenWidth)
	if s.navFolded() {
		side = sidebarIconWidth
	}
	return s.width - side - 10
}

// listWidth is about how wide the current page's table is, beside the
// detail panel when one is open. Before the first frame it is unknown and
// counts as wide.
func (s *desktop) listWidth() float32 {
	if s.width <= 0 {
		return 10000
	}
	w := s.cardWidth()
	if s.selected[s.page] != "" && !s.dockMax {
		w -= s.dockWidth()
	}
	return w
}

// narrow reports whether the current page's table is too narrow for all
// its columns, beside a detail panel or in a small window, so that it shows
// only the ones that matter most.
func (s *desktop) narrow() bool {
	return (s.selected[s.page] != "" && !s.dockMax) || s.listWidth() < narrowListWidth
}

// tight reports whether the table is so narrow that even its state columns
// give way to the names; a row's state still shows on its tile.
func (s *desktop) tight() bool {
	return s.listWidth() < tightListWidth
}

// narrowColumns drops the optional columns, given by index, when narrow
// is set; the rows skip the same cells.
func narrowColumns(cols []column, narrow bool, optional ...int) []column {
	if !narrow {
		return cols
	}
	var out []column
	for i, col := range cols {
		if !slices.Contains(optional, i) {
			out = append(out, col)
		}
	}
	return out
}

// tableHeader draws the column titles. actions is the width the rows keep
// for their buttons, so that titles and cells line up; lead, when set, is
// what stands before the first column, as the rows' checkboxes do.
func tableHeader(c *ui.Context, p palette, cols []column, actions float32, lead func()) {
	ui.Row(c.Key("head")).FillWidth().Height(38).Shrink(0).Padding(0, 14, 0, pageInset).Gap(12).AlignItems(ui.Center).
		Background(p.panel).BorderWidth(1, 0, 1, 0).BorderColor(p.line).Children(func() {
		if lead != nil {
			lead()
		}
		for _, col := range cols {
			cell(c, col.width, col.grow).Children(func() {
				t := ui.Text(c, col.title).FontSize(12).TextColor(p.muted).SingleLine()
				if col.right {
					t.FillWidth().TextAlign(ui.End)
				}
			})
		}
		ui.Box(c).Width(actions).Shrink(0)
	})
}

// tableRow is one selectable row. The cells sit inside a single button that
// selects the row; the action buttons, and lead (a checkbox) when set, stay
// outside it, so they are never nested in another button. The selected row
// takes a faint accent tint.
func (s *desktop) tableRow(c *ui.Context, p palette, id, name string, actionsWidth float32, lead, cells, actions func()) {
	on := s.selected[s.page] == id
	row := ui.Row(c).FillWidth().Height(s.rowHeight()).Shrink(0).AlignItems(ui.Center).
		BorderWidth(0, 0, 1, 0).BorderColor(p.line)
	switch {
	case on:
		row.Background(p.accent.Alpha(0.08))
	case s.picked[id]:
		row.Background(p.accent.Alpha(0.05))
	case row.Hovered():
		row.Background(p.hover)
	}
	// The cells may show something only under the pointer, as a container's
	// checkbox.
	s.rowHovered = row.Hovered()
	row.Children(func() {
		inset := float32(pageInset)
		if lead != nil {
			ui.Row(c.Key("lead")).Shrink(0).Margin(0, 0, 0, pageInset).Children(lead)
			inset = 12
		}
		b := clickable(c.Key("open")).Label(tr("查看 ", "Details for ")+name).Grow(1).Width(0).MinWidth(0).FillHeight().
			Padding(0, 0, 0, inset).Gap(12).AlignItems(ui.Center).Justify(ui.Start)
		b.Children(cells)
		if b.Clicked() {
			s.selectRow(id)
		}
		ui.Row(c.Key("actions")).Width(actionsWidth).Shrink(0).Margin(0, 14, 0, 12).Gap(2).Justify(ui.End).AlignItems(ui.Center).Children(actions)
	})
	s.visible = append(s.visible, id)
}

// dock registers the detail panel of the selected row. It is built after
// the page, to the right of it (see view); body builds the chosen tab.
func (s *desktop) dock(c *ui.Context, p palette, title string, sub func(), tabs []string, actions func(), body func(tab int)) {
	s.detailPanel = func() { s.dockPanel(c, p, title, sub, tabs, actions, body) }
}

// dockPanel is the detail panel beside a list: the title with its state,
// the row's actions, the tabs, then the chosen tab. Dragging its left edge
// resizes it.
func (s *desktop) dockPanel(c *ui.Context, p palette, title string, sub func(), tabs []string, actions func(), body func(tab int)) {
	// A Compose project's panel keeps its own tab, apart from the one its
	// containers' panels share.
	tabKey := s.page
	if strings.HasPrefix(s.selected[s.page], "project:") {
		tabKey += "/project"
	}
	tab := min(max(0, s.tab[tabKey]), len(tabs)-1)
	panel := ui.Row(c.Key("dock")).FillHeight().Shrink(0).AlignItems(ui.Stretch).Background(p.panel)
	if s.dockMax {
		panel.Grow(1).Width(0).MinWidth(0)
	} else {
		panel.Width(s.dockWidth())
	}
	panel.Children(func() {
		grip := ui.Box(c.Key("dock-grip")).Width(5).FillHeight().Shrink(0).Cursor(ui.CursorResizeColumn).
			BorderWidth(0, 0, 0, 1).BorderColor(p.line).Label(tr("调整宽度", "Resize"))
		if !s.dockMax {
			if grip.Hovered() || grip.Pressed() {
				grip.BorderColor(p.accent)
			}
			// Dragged reports the movement since the last frame; dragging
			// left makes the panel wider.
			if dx, _, held := grip.Dragged(); held && dx != 0 {
				s.prefs.DockWidth = min(dockMaxWidth, max(dockMinWidth, s.dockWidth()-dx))
			}
			if _, _, ended := grip.DragEnded(); ended {
				s.savePrefs()
			}
		}
		ui.Column(c.Key("dock-col")).Grow(1).Width(0).MinWidth(0).Children(func() {
			ui.Column(c.Key("dock-head")).FillWidth().Shrink(0).Padding(16, 12, 0, 16).Gap(10).Children(func() {
				ui.Row(c.Key("dock-bar")).FillWidth().Gap(8).AlignItems(ui.Center).Children(func() {
					ui.Text(c, title).FontSize(15.5).FontWeight(600).LetterSpacing(-0.1).TextColor(p.text).SingleLine().Shrink(1).MinWidth(0)
					if sub != nil {
						sub()
					}
					ui.Box(c).Grow(1)
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
				if actions != nil {
					ui.Row(c.Key("dock-actions")).FillWidth().Gap(4).AlignItems(ui.Center).Children(actions)
				}
				ui.Row(c.Key("tabs")).FillWidth().Height(36).Gap(16).BorderWidth(0, 0, 1, 0).BorderColor(p.line).Children(func() {
					for i, name := range tabs {
						b := clickable(c.Key(i)).Label(name).FillHeight().PaddingX(2).Center()
						ink := p.muted
						if i == tab {
							ink = p.text
							b.BorderWidth(0, 0, 2, 0).BorderColor(p.accent)
						} else if b.Hovered() {
							ink = p.text
						}
						b.Children(func() { ui.Text(c, name).FontSize(12.5).FontWeight(500).TextColor(ink).SingleLine() })
						if b.Clicked() {
							s.tab[tabKey] = i
							s.refreshing = false
							s.refresh()
						}
					}
				})
			})
			ui.Column(c.Key("dock-body-" + strconv.Itoa(tab))).FillWidth().Grow(1).MinHeight(0).Children(func() { body(tab) })
		})
	})
}

// keyValues is the body of an info tab.
func keyValues(c *ui.Context, p palette, fields []field) {
	ui.Scroll(c.Key("kv")).Fill().Grow(1).MinHeight(0).Children(func() {
		ui.Column(c).FillWidth().Padding(14, 16).Gap(10).Children(func() {
			for i, f := range fields {
				ui.Row(c.Key(i)).FillWidth().Gap(12).AlignItems(ui.Start).Children(func() {
					ui.Text(c, f.label).Width(96).Shrink(0).FontSize(12).TextColor(p.muted)
					// A web address opens in the browser.
					if strings.HasPrefix(f.value, "http://") || strings.HasPrefix(f.value, "https://") {
						link := clickable(c.Key("open")).Label(fmt.Sprintf(tr("打开 %s", "Open %s"), f.value)).Tooltip(f.value).MinWidth(0).Shrink(1)
						link.Children(func() {
							text := ui.Text(c, f.value).Font(mono).FontSize(11.5).TextColor(p.link)
							if link.Hovered() {
								text.Underline()
							}
						})
						if link.Clicked() {
							c.OpenURL(f.value)
						}
						return
					}
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
				cell(c, 170, 0).Children(func() { s.containerStatus(c, p, x) })
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
		case "machine":
			title, ok = tr("新建 Linux 机器", "New Linux machine"), tr("创建", "Create")
			ui.Text(c, title).FontSize(13.5).FontWeight(600).TextColor(p.text)
			submitted = s.machineForm(c, p)
		case "share":
			title, ok = tr("新建公开链接", "New public link"), tr("共享", "Share")
			ui.Text(c, title).FontSize(13.5).FontWeight(600).TextColor(p.text)
			submitted = s.shareForm(c, p)
		case "share-limit":
			dialog.Width(400).Padding(24).Gap(18).Radius(14)
			ok = tr("升级", "Upgrade")
			s.shareLimitForm(c, p)
		case "share-usage":
			dialog.Width(400).Padding(24).Gap(16).Radius(14)
			ok = tr("升级", "Upgrade")
			if s.shares.billing.Plan.ID != "free" {
				ok = tr("续费", "Renew")
			}
			s.shareUsageForm(c, p)
		case "share-login":
			title, ok = tr("申请公网共享", "Request public sharing"), tr("登录", "Sign in")
			ui.Text(c, title).FontSize(13.5).FontWeight(600).TextColor(p.text)
			submitted = s.shareLoginForm(c, p)
		case "share-relay":
			title, ok = tr("共享中继", "Share relay"), tr("保存", "Save")
			ui.Text(c, title).FontSize(13.5).FontWeight(600).TextColor(p.text)
			submitted = s.shareRelayForm(c, p)
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
			cancel := tr("取消", "Cancel")
			if f.kind == "share-usage" {
				cancel = tr("关闭", "Close")
			}
			if button(c.Key("cancel"), p, cancel, "", plainButton, false) {
				open = false
			}
			submitLabel := ok
			if f.kind == "share-limit" {
				submitLabel = tr("升级连接额度", "Upgrade connection allowance")
			}
			if f.kind == "share-usage" {
				// Distinct from the toolbar button of the same text behind the dialog.
				submitLabel = tr("从流量详情", "From traffic details: ") + ok
			}
			if labeledButton(c.Key("submit"), p, ok, submitLabel, "", primaryButton, false) {
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
	case "machine":
		return s.createMachine()
	case "share":
		return s.createShare()
	case "share-limit", "share-usage":
		s.openShareBilling()
		return true
	case "share-login":
		return s.loginShare()
	case "share-relay":
		return s.setShareRelay()
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
			key, err := registryForImage(ref)
			auth := ""
			if err == nil {
				var login registryLogin
				var found bool
				login, found, err = s.credentials.find(ctx, key)
				if found && err == nil {
					auth = registryHeader(key, login)
				}
			}
			if err == nil {
				err = s.engine.pullAuthenticated(ctx, ref, auth)
			}
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
