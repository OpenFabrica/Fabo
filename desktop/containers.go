package main

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/egoist/mygo/ui"
)

var containerBuckets = []string{"", "running", "stopped", "failed"}

// containerOptional are the columns left out of a narrow table: ports, the
// CPU sparkline, memory and the creation time. The panel shows all of them
// for the selected container. A tight table leaves out containerTight,
// state and CPU as well.
var (
	containerOptional = []int{2, 4, 5, 6}
	containerTight    = []int{1, 2, 3, 4, 5, 6}
)

// containerColumns are shared by the header and the rows. The name cell
// carries the image too, on a second line.
func containerColumns() []column {
	return []column{
		{tr("名称", "Name"), 0, 1, false}, {tr("状态", "Status"), 150, 0, false},
		{tr("端口", "Ports"), 104, 0, false}, {"CPU", 52, 0, true}, {"", 58, 0, false},
		{tr("内存", "Memory"), 64, 0, true}, {tr("创建", "Created"), 76, 0, false},
	}
}

const containerActionsWidth = 92

// treeWidth is the room before a row's tile: a group's fold arrow, and the
// guide that hangs a member under its group.
const treeWidth = 22

// projectKey is the selection key of a Compose project's row.
func projectKey(name string) string { return "project:" + name }

// containerRow is one line of the flattened list: a Compose project's
// heading with its members, or a container. last marks a project's final
// member, where the tree guide ends.
type containerRow struct {
	project string
	members []container
	c       container
	last    bool
}

// containerDot is the bullet of a container, amber while a change is pending.
func (s *desktop) containerDot(c *ui.Context, p palette, x container) {
	if s.working[x.ID] {
		dot(c, p, "busy")
		return
	}
	dot(c, p, x.bucket())
}

// containerActions are the start, or restart and stop, buttons of a container.
func (s *desktop) containerActions(c *ui.Context, p palette, x container) {
	busy := s.working[x.ID]
	if x.bucket() != "running" {
		if iconButton(c.Key("start"), p, "play", tr("启动 ", "Start ")+x.Name, busy).Clicked() {
			s.startContainer(x)
		}
		return
	}
	if iconButton(c.Key("restart"), p, "restart", tr("重启 ", "Restart ")+x.Name, busy).Clicked() {
		s.restartContainer(x)
	}
	if iconButton(c.Key("stop"), p, "stop", tr("停止 ", "Stop ")+x.Name, busy).Clicked() {
		s.stopContainer(x)
	}
}

// pickState is how many of the given containers are picked: none (0),
// some (1) or all (2), as a checkbox shows it.
func (s *desktop) pickState(list []container) int {
	n := 0
	for _, x := range list {
		if s.picked[x.ID] {
			n++
		}
	}
	switch {
	case n == 0:
		return 0
	case n < len(list):
		return 1
	}
	return 2
}

// togglePicked picks all of the given containers, or drops them all when
// every one is picked already.
func (s *desktop) togglePicked(list []container) {
	all := s.pickState(list) == 2
	for _, x := range list {
		if all {
			delete(s.picked, x.ID)
		} else {
			s.picked[x.ID] = true
		}
	}
}

// containersView is the containers page: filters, or the actions on the
// picked containers, the grouped table, and the selected row's panel.
func (s *desktop) containersView(c *ui.Context, p palette) {
	query := strings.ToLower(strings.TrimSpace(s.search))
	filter := containerBuckets[s.filter[containersPage]]
	counts := map[string]int{}
	exists := map[string]bool{}
	var shown, dead []container
	for _, x := range s.containers {
		exists[x.ID] = true
		counts[x.bucket()]++
		if x.bucket() != "running" {
			dead = append(dead, x)
		}
		if (filter == "" || x.bucket() == filter) && strings.Contains(strings.ToLower(x.Name+" "+x.Image+" "+x.Project), query) {
			shown = append(shown, x)
		}
	}
	// A container that went away cannot stay picked.
	for id := range s.picked {
		if !exists[id] {
			delete(s.picked, id)
		}
	}
	// Flatten into list rows. A project's heading comes where its first
	// member is, followed by its members unless the project is folded.
	var rows []containerRow
	heading := map[string]int{}
	for _, x := range shown {
		if x.Project == "" {
			rows = append(rows, containerRow{c: x})
			continue
		}
		at, ok := heading[x.Project]
		if !ok {
			at = len(rows)
			heading[x.Project] = at
			rows = append(rows, containerRow{project: x.Project})
		}
		rows[at].members = append(rows[at].members, x)
	}
	flat := make([]containerRow, 0, len(shown)+len(heading))
	for _, r := range rows {
		flat = append(flat, r)
		if r.project != "" && !s.collapsed[r.project] {
			for i, m := range r.members {
				flat = append(flat, containerRow{c: m, last: i == len(r.members)-1})
			}
		}
	}

	toolbar(c, func() {
		if len(s.picked) > 0 {
			s.pickedBar(c, p, shown)
			return
		}
		labels := []string{tr("全部", "All"), tr("运行中", "Running"), tr("已停止", "Stopped"), tr("异常", "Failed")}
		n := []int{len(s.containers), counts["running"], counts["stopped"], counts["failed"]}
		at := s.filter[containersPage]
		if filters(c.Key("filter"), p, tr("按状态筛选", "Filter by state"), &at, labels, n) {
			s.filter[containersPage] = at
			s.list(containersPage).ScrollTo(0, ui.Start)
		}
		ui.Box(c).Grow(1)
		// The filters take about 290, Remove stopped about 110 and Run
		// container about 120; a narrower table drops the first button,
		// then keeps only the icon of the second.
		wide := s.listWidth()
		if len(dead) > 0 && wide >= 620 && button(c.Key("prune"), p, fmt.Sprintf(tr("清理已停止 %d", "Remove stopped %d"), len(dead)), "", ghostButton, false) {
			s.pending = &confirmation{
				title: fmt.Sprintf(tr("清理 %d 个已停止的容器？", "Remove %d stopped containers?"), len(dead)), ok: tr("清理", "Remove"),
				run: func() {
					s.do("prune", engineAction{"POST", "/containers/prune", nil}, tr("已清理已停止的容器", "Stopped containers removed"))
				},
			}
		}
		run := tr("运行容器", "Run container")
		if wide < 480 {
			b := ui.ButtonBase(c.Key("run")).Label(run).Tooltip(run).Size(32, 32).Shrink(0).Radius(8).Center().
				Cursor(ui.CursorPointer).Background(p.accent)
			if b.Hovered() {
				b.Background(p.accent.Mix(p.text, 0.1))
			}
			b.Children(func() { ui.Icon(c, icons["plus"]).Size(16, 16).TextColor(p.onAccent) })
			if b.Clicked() {
				s.form = form{kind: "run"}
			}
		} else if button(c.Key("run"), p, run, "plus", primaryButton, false) {
			s.form = form{kind: "run"}
		}
	})
	if !s.dockMax {
		optional := containerOptional
		if s.tight() {
			optional = containerTight
		}
		tableHeader(c, p, narrowColumns(containerColumns(), s.narrow(), optional...), containerActionsWidth, func() {
			if checkbox(c.Key("pick-all"), p, tr("全选", "Select all"), s.pickState(shown)) {
				s.togglePicked(shown)
			}
		})
		switch {
		case len(flat) > 0:
			ui.List(c.Key("rows"), s.list(containersPage), len(flat)).FillWidth().Grow(1).MinHeight(0).
				ItemKey(func(i int) any {
					if flat[i].project != "" {
						return projectKey(flat[i].project)
					}
					return flat[i].c.ID
				}).
				Rows(func(r ui.ListRow) {
					if row := flat[r.Index]; row.project != "" {
						s.projectRow(r.Context, p, row)
					} else {
						s.containerRow(r.Context, p, row)
					}
				})
		case len(s.containers) > 0:
			empty(c, p, "search", tr("没有匹配的容器", "No matching containers"), "")
		default:
			if empty(c, p, "box", tr("还没有容器", "No containers yet"), tr("运行容器", "Run container")) {
				s.form = form{kind: "run"}
			}
		}
	}
	if name, ok := strings.CutPrefix(s.selected[containersPage], "project:"); ok {
		var members []container
		for _, x := range s.containers {
			if x.Project == name {
				members = append(members, x)
			}
		}
		if len(members) > 0 {
			s.projectDock(c, p, name, members)
		}
	} else if x, ok := s.container(s.selected[containersPage]); ok {
		s.containerDock(c, p, x)
	}
}

// pickedBar takes the toolbar's place while containers are picked: how
// many, a way to pick every container shown, and what can be done to them.
func (s *desktop) pickedBar(c *ui.Context, p palette, shown []container) {
	var picked []container
	running := 0
	for _, x := range s.containers {
		if s.picked[x.ID] {
			picked = append(picked, x)
			if x.bucket() == "running" {
				running++
			}
		}
	}
	ui.Row(c.Key("picked")).Height(34).Shrink(0).Padding(0, 4, 0, 12).Gap(6).Radius(10).AlignItems(ui.Center).
		Background(p.accent.Alpha(0.1)).Children(func() {
		ui.Text(c, fmt.Sprintf(tr("已选 %d 项", "%d selected"), len(picked))).FontSize(12.5).FontWeight(500).FontFeatures("tnum").TextColor(p.text)
		if iconButton(c.Key("unpick"), p, "x", tr("清除选择", "Clear selection"), false).Clicked() {
			clear(s.picked)
		}
	})
	if s.pickState(shown) != 2 {
		all := clickable(c.Key("pick-shown")).Label(tr("全选", "Select all")).Height(32).PaddingX(8).Radius(8).Center()
		if all.Hovered() {
			all.Background(p.soft)
		}
		all.Children(func() {
			ui.Text(c, fmt.Sprintf(tr("全选 %d 项", "Select all %d"), len(shown))).FontSize(12.5).FontWeight(500).TextColor(p.accent)
		})
		if all.Clicked() {
			for _, x := range shown {
				s.picked[x.ID] = true
			}
		}
	}
	ui.Box(c).Grow(1)
	if button(c.Key("start-picked"), p, tr("启动", "Start"), "play", plainButton, running == len(picked)) {
		for _, x := range picked {
			if x.bucket() != "running" {
				s.startContainer(x)
			}
		}
	}
	if button(c.Key("stop-picked"), p, tr("停止", "Stop"), "stop", plainButton, running == 0) {
		for _, x := range picked {
			if x.bucket() == "running" {
				s.stopContainer(x)
			}
		}
	}
	if button(c.Key("delete-picked"), p, tr("删除", "Delete"), "trash", removeButton, false) {
		s.removeContainers(picked, "")
	}
}

// removeContainers asks once before removing several containers, forcing
// the running ones; title, when set, replaces the question.
func (s *desktop) removeContainers(list []container, title string) {
	running := 0
	for _, x := range list {
		if x.bucket() == "running" {
			running++
		}
	}
	if title == "" {
		title = fmt.Sprintf(tr("删除 %d 个容器？", "Delete %d containers?"), len(list))
	}
	q := &confirmation{title: title, ok: tr("删除", "Delete")}
	if running > 0 {
		q.body = fmt.Sprintf(tr("其中 %d 个正在运行，会被强制停止。", "%d of them are running and will be force-stopped."), running)
	}
	q.run = func() {
		for _, x := range list {
			path := containerPath(x.ID) + "?v=0"
			if x.bucket() == "running" {
				path += "&force=1"
			}
			delete(s.picked, x.ID)
			s.do(x.ID, engineAction{http.MethodDelete, path, nil}, "")
		}
	}
	s.pending = q
}

// treeGuide is the line that hangs a project's member under the project's
// fold arrow: down the row, or to its middle on the last member, with a
// short branch toward the member's tile.
func treeGuide(c *ui.Context, p palette, last bool) {
	ink := p.muted.Alpha(0.35)
	ui.Box(c).Width(treeWidth).FillHeight().Shrink(0).Draw(func(pt *ui.Painter, r ui.Rect) {
		x, mid := r.X+7, r.Y+r.H/2
		bottom := r.Y + r.H
		if last {
			bottom = mid
		}
		pt.Line(x, r.Y, x, bottom, 1, ink)
		pt.Line(x, mid, r.X+treeWidth-4, mid, 1, ink)
	})
}

// projectRow is the heading of a Compose project. Its checkbox picks the
// members shown, the arrow folds them, and the rest opens the project's
// panel; the buttons act on all of its members.
func (s *desktop) projectRow(c *ui.Context, p palette, row containerRow) {
	key := projectKey(row.project)
	running, stopped := 0, 0
	for _, m := range row.members {
		if m.bucket() == "running" {
			running++
		} else {
			stopped++
		}
	}
	line := ui.Row(c).FillWidth().Height(s.rowHeight()).Shrink(0).AlignItems(ui.Center).BorderWidth(0, 0, 1, 0).BorderColor(p.line)
	switch {
	case s.selected[containersPage] == key:
		line.Background(p.accent.Alpha(0.08))
	case line.Hovered():
		line.Background(p.hover)
	default:
		line.Background(p.hover.Mix(p.panel, 0.4))
	}
	line.Children(func() {
		ui.Row(c.Key("lead")).Shrink(0).Margin(0, 12, 0, pageInset).Children(func() {
			if checkbox(c.Key("pick"), p, tr("选择 ", "Select ")+row.project, s.pickState(row.members)) {
				s.togglePicked(row.members)
			}
		})
		fold := clickable(c.Key("fold")).Label(tr("折叠 ", "Fold ") + row.project).Width(treeWidth).FillHeight().Shrink(0).Justify(ui.Start).AlignItems(ui.Center)
		fold.Children(func() {
			arrow := "down"
			if s.collapsed[row.project] {
				arrow = "chevron"
			}
			ink := p.muted
			if fold.Hovered() {
				ink = p.text
			}
			ui.Icon(c, icons[arrow]).Size(14, 14).TextColor(ink)
		})
		if fold.Clicked() {
			s.collapsed[row.project] = !s.collapsed[row.project]
		}
		open := clickable(c.Key("open")).Label(tr("查看 ", "Details for ") + row.project).Grow(1).Width(0).MinWidth(0).FillHeight().
			Gap(10).AlignItems(ui.Center).Justify(ui.Start)
		open.Children(func() {
			imageTile(c, p, "", "layers")
			ui.Column(c).Grow(1).Width(0).MinWidth(0).Gap(2).Children(func() {
				ui.Text(c, row.project).FontSize(13).FontWeight(600).TextColor(p.text).SingleLine()
				ui.Text(c, fmt.Sprintf(tr("Compose · %d/%d 运行中", "Compose · %d/%d running"), running, len(row.members))).
					FontSize(12).FontFeatures("tnum").TextColor(p.muted).SingleLine()
			})
		})
		if open.Clicked() {
			s.selectRow(key)
		}
		ui.Row(c.Key("actions")).Shrink(0).Margin(0, 14, 0, 12).Gap(2).Children(func() {
			each := func(want bool, act func(container)) {
				for _, m := range row.members {
					if (m.bucket() == "running") == want {
						act(m)
					}
				}
			}
			if stopped > 0 && iconButton(c.Key("start-all"), p, "play", tr("全部启动 ", "Start all in ")+row.project, false).Clicked() {
				each(false, s.startContainer)
			}
			if running > 0 && iconButton(c.Key("restart-all"), p, "restart", tr("全部重启 ", "Restart all in ")+row.project, false).Clicked() {
				each(true, s.restartContainer)
			}
			if running > 0 && iconButton(c.Key("stop-all"), p, "stop", tr("全部停止 ", "Stop all in ")+row.project, false).Clicked() {
				each(true, s.stopContainer)
			}
		})
	})
	s.visible = append(s.visible, key)
}

// containerRow is one container of the table: its checkbox, then its tile,
// name and image, hung under its project when it has one.
func (s *desktop) containerRow(c *ui.Context, p palette, row containerRow) {
	x := row.c
	cols := containerColumns()
	u := s.usage[x.ID]
	live := x.State == "running" && u != nil
	pick := func() {
		if checkbox(c.Key("pick"), p, tr("选择 ", "Select ")+x.Name, s.pickState([]container{x})) {
			s.togglePicked([]container{x})
		}
	}
	s.tableRow(c, p, x.ID, x.Name, containerActionsWidth, pick, func() {
		narrow, tight := s.narrow(), s.tight()
		// The name keeps room for its tile and a few letters however
		// narrow the table gets.
		cell(c, cols[0].width, cols[0].grow).FillHeight().MinWidth(150).Gap(10).Children(func() {
			if x.Project != "" {
				treeGuide(c, p, row.last)
			} else {
				ui.Box(c).Width(treeWidth).Shrink(0)
			}
			ui.Box(c).Shrink(0).Children(func() {
				imageTile(c, p, x.Image, "box")
				// The state sits on the tile's corner, as on an avatar.
				ui.Box(c).Absolute().Right(-3).Bottom(-3).Padding(2).Radius(6).Background(p.panel).Children(func() {
					s.containerDot(c, p, x)
				})
			})
			ui.Column(c).Grow(1).Width(0).MinWidth(0).Gap(2).Children(func() {
				ui.Text(c, x.Name).FontSize(13).FontWeight(500).TextColor(p.text).SingleLine()
				label(c, x.Image, p.muted)
			})
		})
		if tight {
			return
		}
		cell(c, cols[1].width, 0).Children(func() { label(c, x.status(s.now), stateColor(p, x.bucket())) })
		if !narrow {
			cell(c, cols[2].width, 0).Children(func() {
				if len(x.Ports) == 0 {
					return
				}
				text := fmt.Sprintf("%d→%d", x.Ports[0].Public, x.Ports[0].Private)
				if len(x.Ports) > 1 {
					text += fmt.Sprintf(" +%d", len(x.Ports)-1)
				}
				label(c, text, p.link)
			})
		}
		cell(c, cols[3].width, 0).Children(func() {
			if live {
				number(c, fmt.Sprintf("%.1f%%", u.cpu), p.text)
			} else {
				number(c, "—", p.muted)
			}
		})
		if narrow {
			return
		}
		if live {
			spark(c, p, u.hist, cols[4].width)
		} else {
			ui.Box(c).Width(cols[4].width).Shrink(0)
		}
		cell(c, cols[5].width, 0).Children(func() {
			if live {
				number(c, displaySize(int64(u.memory)), p.text)
			} else {
				number(c, "—", p.muted)
			}
		})
		cell(c, cols[6].width, 0).Children(func() { label(c, since(s.now, x.Created), p.muted) })
	}, func() {
		s.containerActions(c, p, x)
		iconButton(c.Key("more"), p, "more", tr("更多操作 ", "More actions for ")+x.Name, false).PopoverMenuButton(func(m *ui.Menu) {
			show := func(tab int) {
				s.selectRow(x.ID)
				s.tab[containersPage] = tab
			}
			if m.Item(tr("日志", "Logs")).Chosen() {
				show(1)
			}
			if m.Item(tr("终端", "Terminal")).Chosen() {
				show(2)
			}
			if m.Item(tr("文件", "Files")).Chosen() {
				show(3)
			}
			if m.Item(tr("复制 ID", "Copy ID")).Chosen() {
				c.WriteClipboard(x.ID)
				s.notices = append(s.notices, tr("已复制", "Copied"))
			}
			m.Separator()
			if m.Item(tr("删除…", "Delete…")).Danger().Chosen() {
				s.removeContainer(x)
			}
		})
	})
}

// containerDock shows the selected container: info, logs, a command line and
// its files.
func (s *desktop) containerDock(c *ui.Context, p palette, x container) {
	key := containersPage + "/" + x.ID
	if s.detail.key != key {
		s.detail = detail{key: key}
		s.loadDetail(key)
	}
	tabs := []string{tr("信息", "Info"), tr("日志", "Logs"), tr("终端", "Terminal"), tr("文件", "Files"), tr("设置", "Container settings")}
	s.dock(c, p, x.Name, func() {
		label(c, x.status(s.now), stateColor(p, x.bucket())).Shrink(0)
	}, tabs, func() {
		if s.tab[containersPage] == 1 {
			if iconButton(c.Key("reconnect-logs"), p, "refresh", tr("重新连接日志", "Reconnect logs"), false).Clicked() {
				if s.containerLogs != nil {
					s.retireLogs(s.containerLogs)
					s.containerLogs = nil
				}
			}
			if iconButton(c.Key("copy-logs"), p, "copy", tr("复制日志", "Copy logs"), false).Clicked() {
				lines := s.visibleContainerLogs().lines
				c.WriteClipboard(strings.Join(lines, "\n"))
				s.notices = append(s.notices, tr("已复制", "Copied"))
			}
		}
		if len(x.Ports) > 0 && x.State == "running" {
			if iconButton(c.Key("browse"), p, "out", tr("在浏览器打开", "Open in browser"), false).Clicked() {
				c.OpenURL(fmt.Sprintf("http://localhost:%d", x.Ports[0].Public))
			}
		}
		s.containerActions(c, p, x)
		if iconButton(c.Key("delete"), p, "trash", tr("删除 ", "Delete ")+x.Name, s.working[x.ID]).Clicked() {
			s.removeContainer(x)
		}
	}, func(tab int) {
		if tab != 2 {
			s.dropExitedTerminal(x)
		}
		switch tab {
		case 0:
			s.usageCards(c, p, x)
			keyValues(c, p, s.detail.fields)
		case 1:
			s.logsTab(c, p)
		case 2:
			s.interactiveTerminalTab(c, p, x)
		case 3:
			s.filesTab(c, p, x)
		case 4:
			s.containerSettingsTab(c, p, x)
		}
	})
}

// usageCards head the info tab of a running container: its CPU with the
// recent history, and its memory against the engine's total.
func (s *desktop) usageCards(c *ui.Context, p palette, x container) {
	u := s.usage[x.ID]
	if x.State != "running" || u == nil {
		return
	}
	card := func(key, title, figure string, body func()) {
		ui.Column(c.Key(key)).Grow(1).Width(0).MinWidth(0).Padding(12).Gap(10).Radius(10).Background(p.hover).Children(func() {
			ui.Row(c).FillWidth().Children(func() {
				ui.Text(c, title).FontSize(12).TextColor(p.muted).Grow(1)
				ui.Text(c, figure).Font(mono).FontSize(12).FontFeatures("tnum").TextColor(p.text)
			})
			body()
		})
	}
	ui.Row(c.Key("usage")).FillWidth().Shrink(0).Padding(14, 16, 0).Gap(10).Children(func() {
		card("cpu", "CPU", fmt.Sprintf("%.1f%%", u.cpu), func() { spark(c, p, u.hist, 140) })
		_, total := s.memoryUse()
		card("memory", tr("内存", "Memory"), displaySize(int64(u.memory)), func() {
			ui.Box(c).FillWidth().Height(14).Center().Children(func() {
				meter(c, p, float64(u.memory)/float64(max(1, total)), 6, p.ok)
			})
		})
	})
}

// Log levels a line can be told apart by, in the order the level picker
// lists them after "All levels".
const (
	logAny = iota
	logError
	logWarn
	logInfo
	logDebug
)

// logWords are the words that mark a line's level, lowercased.
var logWords = []struct {
	word  string
	level int
}{
	{"error", logError}, {"fatal", logError}, {"panic", logError}, {"crit", logError}, {"exception", logError},
	{"emerg", logError}, {"[err", logError}, {"level=err", logError}, {" err ", logError},
	{"warn", logWarn}, {"info", logInfo}, {"notice", logInfo}, {"debug", logDebug}, {"trace", logDebug},
}

// logLevel tells a line's level by whichever marking word comes first, so
// that a warning which mentions a failure stays a warning. A line with no
// such word has no level.
func logLevel(line string) int {
	lower := strings.ToLower(line)
	level, at := logAny, len(lower)
	for _, w := range logWords {
		if i := strings.Index(lower, w.word); i >= 0 && i < at {
			level, at = w.level, i
		}
	}
	return level
}

// The colors of the dark log well, the same in light and dark windows.
var (
	logInk     = ui.Hex("#E6EDF3")
	logGutter  = ui.Hex("#6E7681")
	logWarnInk = ui.Hex("#E3B341")
	logErrInk  = ui.Hex("#F47067")
	logDimInk  = ui.Hex("#8B949E")
)

// logWell draws log lines on the console's dark ground: each with its
// number, long ones wrapped and, with levels, warnings, errors and debug
// lines in their own colors. With follow it stays at the end as lines
// arrive. A file's preview uses it too, without levels.
func (s *desktop) logWell(c *ui.Context, name string, lines []string, numbers []int, follow, levels bool) {
	state := s.list(name)
	state.FollowEnd = follow
	gutter := float32(len(fmt.Sprint(numbers[len(numbers)-1]))) * s.prefs.LogSize * 0.62
	ui.List(c.Key(name), state, len(lines)).Grow(1).MinHeight(0).Margin(0, 16, 16).Padding(10, 0).
		Radius(10).Background(consoleTheme.Background).Rows(func(r ui.ListRow) {
		line := lines[r.Index]
		ink, level := logInk, logAny
		if levels {
			level = logLevel(line)
		}
		switch level {
		case logError:
			ink = logErrInk
		case logWarn:
			ink = logWarnInk
		case logDebug:
			ink = logDimInk
		}
		ui.Row(r.Context).FillWidth().PaddingX(12).Gap(12).AlignItems(ui.Start).Children(func() {
			ui.Textf(r.Context, "%d", numbers[r.Index]).Font(mono).FontSize(s.prefs.LogSize).LineHeight(1.6).
				FontFeatures("tnum").TextColor(logGutter).Width(gutter).Shrink(0).TextAlign(ui.End)
			ui.Text(r.Context, line).Font(mono).FontSize(s.prefs.LogSize).LineHeight(1.6).TextColor(ink).
				Width(0).Grow(1).MinWidth(0).Selectable()
		})
	})
}

// numbered pairs lines with their numbers, counting from first.
func numbered(lines []string, first int) []int {
	numbers := make([]int, len(lines))
	for i := range numbers {
		numbers[i] = first + i
	}
	return numbers
}

// logsTab shows the container's output under a bar to find text, keep
// one level, follow the end and clear what was read.
func (s *desktop) logsTab(c *ui.Context, p palette) {
	source := s.visibleContainerLogs()
	query := strings.ToLower(strings.TrimSpace(s.logFilter))
	var shown []string
	var numbers []int
	for i, line := range source.lines {
		if (query == "" || strings.Contains(strings.ToLower(line), query)) && (s.logLevel == logAny || logLevel(line) == s.logLevel) {
			shown = append(shown, line)
			numbers = append(numbers, source.first+i)
		}
	}
	ui.Row(c.Key("log-bar")).FillWidth().Shrink(0).Padding(12, 16, 10).Gap(8).AlignItems(ui.Center).Children(func() {
		ui.TextInput(c.Key("log-filter"), &s.logFilter).Label(tr("在日志中查找", "Find in logs")).Placeholder(tr("在日志中查找", "Find in logs")).
			Width(0).Grow(1).MinWidth(70).Height(32).Radius(8).FontSize(12.5)
		levels := []string{tr("全部级别", "All levels"), tr("错误", "Errors"), tr("警告", "Warnings"), tr("信息", "Info"), tr("调试", "Debug")}
		chosen := levels[s.logLevel]
		if ui.Select(c.Key("log-level"), &chosen, levels).Label(tr("日志级别", "Log level")).Height(32).MinWidth(96).Shrink(0).
			Cursor(ui.CursorPointer).Changed() {
			for i, l := range levels {
				if l == chosen {
					s.logLevel = i
				}
			}
		}
		// The count gives way first when the panel is narrow.
		if s.dockMax || s.dockWidth() >= 440 {
			ui.Text(c, fmt.Sprintf(tr("%d 行", "%d lines"), len(shown))).FontSize(12).FontFeatures("tnum").TextColor(p.muted).Shrink(0)
		}
		ui.Box(c).Grow(0.001)
		// Follow is a ticked box with its word, both one button.
		follow := clickable(c.Key("follow")).Label(tr("跟随", "Follow")).Height(32).Gap(6).AlignItems(ui.Center).Shrink(0)
		follow.Children(func() {
			box := ui.Box(c).Size(16, 16).Radius(4).Center()
			if s.follow {
				box.Background(p.accent).Children(func() { ui.Icon(c, icons["check"]).Size(12, 12).TextColor(p.onAccent) })
			} else {
				box.Background(p.panel).Border(1.5, p.muted.Alpha(0.5))
			}
			ui.Text(c, tr("跟随", "Follow")).FontSize(12.5).TextColor(p.sub)
		})
		if follow.Clicked() {
			s.follow = !s.follow
		}
		if button(c.Key("clear-logs"), p, tr("清除", "Clear"), "", plainButton, len(source.lines) == 0) && s.containerLogs != nil {
			s.containerLogs.clear()
		}
	})
	if source.problem != "" {
		remark(c, p, source.problem)
	}
	if len(shown) == 0 {
		text := tr("暂无日志", "No logs yet")
		if len(source.lines) > 0 {
			text = tr("没有匹配的日志", "No matching lines")
		}
		note(c, p, text)
		return
	}
	s.logWell(c, "logs", shown, numbers, s.follow, true)
}
