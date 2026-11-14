package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/egoist/mygo/ui"
)

var containerBuckets = []string{"", "running", "stopped", "failed"}

// containerColumns are shared by the header and the rows.
func containerColumns() []column {
	return []column{
		{tr("名称", "Name"), 0, 1.1, false}, {tr("镜像", "Image"), 0, 1, false}, {tr("状态", "Status"), 170, 0, false},
		{tr("端口", "Ports"), 104, 0, false}, {"CPU", 52, 0, true}, {"", 58, 0, false},
		{tr("内存", "Memory"), 64, 0, true}, {tr("创建", "Created"), 76, 0, false},
	}
}

const containerActionsWidth = 76

// containerRow is one line of the flattened list: a Compose project's
// heading with its members, or a container.
type containerRow struct {
	project string
	members []container
	c       container
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

// containersView is the containers page: filters, the grouped table, and the
// dock of the selected container.
func (s *desktop) containersView(c *ui.Context, p palette) {
	query := strings.ToLower(strings.TrimSpace(s.search))
	filter := containerBuckets[s.filter[containersPage]]
	counts := map[string]int{}
	var shown, dead []container
	for _, x := range s.containers {
		counts[x.bucket()]++
		if x.bucket() != "running" {
			dead = append(dead, x)
		}
		if (filter == "" || x.bucket() == filter) && strings.Contains(strings.ToLower(x.Name+" "+x.Image), query) {
			shown = append(shown, x)
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
			for _, m := range r.members {
				flat = append(flat, containerRow{c: m})
			}
		}
	}

	toolbar(c, func() {
		labels := []string{tr("全部", "All"), tr("运行中", "Running"), tr("已停止", "Stopped"), tr("异常", "Failed")}
		for i, name := range labels {
			n := len(s.containers)
			if i > 0 {
				n = counts[containerBuckets[i]]
			}
			if pill(c.Key("filter-"+containerBuckets[i]), p, name, n, s.filter[containersPage] == i) {
				s.filter[containersPage] = i
				s.list(containersPage).ScrollTo(0, ui.Start)
			}
		}
		ui.Box(c).Grow(1)
		if len(dead) > 0 && button(c.Key("prune"), p, fmt.Sprintf(tr("清理已停止 %d", "Remove stopped %d"), len(dead)), "", ghostButton, false) {
			s.pending = &confirmation{
				title: fmt.Sprintf(tr("清理 %d 个已停止的容器？", "Remove %d stopped containers?"), len(dead)), ok: tr("清理", "Remove"),
				run: func() {
					s.do("prune", engineAction{"POST", "/containers/prune", nil}, tr("已清理已停止的容器", "Stopped containers removed"))
				},
			}
		}
		if button(c.Key("run"), p, tr("运行容器", "Run container"), "plus", primaryButton, false) {
			s.form = form{kind: "run"}
		}
	})
	if !s.dockMax {
		tableHeader(c, p, containerColumns(), containerActionsWidth)
		switch {
		case len(flat) > 0:
			ui.List(c.Key("rows"), s.list(containersPage), len(flat)).FillWidth().Grow(1).MinHeight(0).
				ItemKey(func(i int) any {
					if flat[i].project != "" {
						return "project:" + flat[i].project
					}
					return flat[i].c.ID
				}).
				Rows(func(r ui.ListRow) {
					if row := flat[r.Index]; row.project != "" {
						s.projectRow(r.Context, p, row)
					} else {
						s.containerRow(r.Context, p, row.c)
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
	if x, ok := s.container(s.selected[containersPage]); ok {
		s.containerDock(c, p, x)
	}
}

// projectRow is the heading of a Compose project: a fold switch, the number
// of members running, and actions on all of them.
func (s *desktop) projectRow(c *ui.Context, p palette, row containerRow) {
	running, stopped := 0, 0
	for _, m := range row.members {
		if m.bucket() == "running" {
			running++
		} else {
			stopped++
		}
	}
	ui.Row(c).FillWidth().Height(s.rowHeight()-2).Shrink(0).AlignItems(ui.Center).Background(p.panel).
		BorderWidth(0, 0, 1, 0).BorderColor(p.line).Children(func() {
		fold := clickable(c.Key("fold")).Label(tr("折叠 ", "Fold ")+row.project).Grow(1).Width(0).MinWidth(0).FillHeight().
			Padding(0, 0, 0, 12).Gap(8).AlignItems(ui.Center).Justify(ui.Start)
		fold.Children(func() {
			arrow := "down"
			if s.collapsed[row.project] {
				arrow = "chevron"
			}
			ui.Icon(c, icons[arrow]).Size(12, 12).Shrink(0).TextColor(p.muted)
			ui.Text(c, row.project).Font(mono).FontSize(11.5).Bold().TextColor(p.text).SingleLine()
			label(c, fmt.Sprintf("compose · %d/%d", running, len(row.members)), p.muted)
		})
		if fold.Clicked() {
			s.collapsed[row.project] = !s.collapsed[row.project]
		}
		ui.Row(c.Key("actions")).Shrink(0).Margin(0, 10, 0, 10).Gap(1).Children(func() {
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
}

// containerRow is one container of the table.
func (s *desktop) containerRow(c *ui.Context, p palette, x container) {
	cols := containerColumns()
	u := s.usage[x.ID]
	live := x.State == "running" && u != nil
	s.tableRow(c, p, x.ID, x.Name, containerActionsWidth, func() {
		cell(c, cols[0].width, cols[0].grow).Gap(8).Children(func() {
			if x.Project != "" {
				ui.Box(c).Width(10).Shrink(0)
			}
			s.containerDot(c, p, x)
			label(c, x.Name, p.text).FontWeight(500)
		})
		cell(c, cols[1].width, cols[1].grow).Children(func() { label(c, x.Image, p.muted) })
		cell(c, cols[2].width, 0).Children(func() { label(c, x.status(s.now), stateColor(p, x.bucket())) })
		cell(c, cols[3].width, 0).Children(func() {
			if len(x.Ports) == 0 {
				return
			}
			text := fmt.Sprintf("%d→%d", x.Ports[0].Public, x.Ports[0].Private)
			if len(x.Ports) > 1 {
				text += fmt.Sprintf(" +%d", len(x.Ports)-1)
			}
			label(c, text, p.link)
		})
		cell(c, cols[4].width, 0).Children(func() {
			if live {
				number(c, fmt.Sprintf("%.1f%%", u.cpu), p.text)
			} else {
				number(c, "—", p.muted)
			}
		})
		if live {
			spark(c, p, u.hist, cols[5].width)
		} else {
			ui.Box(c).Width(cols[5].width).Shrink(0)
		}
		cell(c, cols[6].width, 0).Children(func() {
			if live {
				number(c, displaySize(int64(u.memory)), p.text)
			} else {
				number(c, "—", p.muted)
			}
		})
		cell(c, cols[7].width, 0).Children(func() { label(c, since(s.now, x.Created), p.muted) })
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
			ui.TextInput(c.Key("log-filter"), &s.logFilter).Label(tr("筛选日志", "Filter logs")).Placeholder(tr("筛选日志", "Filter logs")).
				Width(130).Height(22).FontSize(11.5)
			follow := clickable(c.Key("follow")).Label(tr("跟随", "Follow")).Height(22).PaddingX(8).Radius(4).Center()
			ink := p.muted
			if s.follow {
				ink = p.accent
			}
			follow.Border(1, ink.Alpha(0.7)).Children(func() { ui.Text(c, tr("跟随", "Follow")).FontSize(11.5).TextColor(ink) })
			if follow.Clicked() {
				s.follow = !s.follow
			}
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
		switch tab {
		case 0:
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

// lines draws text lines in a virtual list that can stay at its end.
func (s *desktop) lines(c *ui.Context, p palette, name string, lines []string, follow bool) {
	state := s.list(name)
	state.FollowEnd = follow
	ui.List(c.Key(name), state, len(lines)).FillWidth().Grow(1).MinHeight(0).Padding(8, 0).Rows(func(r ui.ListRow) {
		ink := p.text
		if strings.HasPrefix(lines[r.Index], "$ ") {
			ink = p.accent
		}
		ui.Text(r.Context, lines[r.Index]).Font(mono).FontSize(s.prefs.LogSize).LineHeight(1.6).TextColor(ink).
			FillWidth().PaddingX(14).SingleLine().Ellipsis("…")
	})
}

// logsTab shows the tail of the container's output, narrowed by the filter.
func (s *desktop) logsTab(c *ui.Context, p palette) {
	source := s.visibleContainerLogs()
	if source.problem != "" {
		remark(c, p, source.problem)
	}
	shown := source.lines
	if q := strings.ToLower(strings.TrimSpace(s.logFilter)); q != "" {
		shown = nil
		for _, line := range source.lines {
			if strings.Contains(strings.ToLower(line), q) {
				shown = append(shown, line)
			}
		}
	}
	if len(shown) == 0 {
		text := tr("暂无日志", "No logs yet")
		if len(source.lines) > 0 {
			text = tr("没有匹配的日志", "No matching lines")
		}
		note(c, p, text)
		return
	}
	s.lines(c, p, "logs", shown, s.follow)
}

// filesTab browses the container's filesystem by listing one directory at a
// time with ls, which needs the container running.
func (s *desktop) filesTab(c *ui.Context, p palette, x container) {
	if x.State != "running" {
		if empty(c, p, "folder", tr("容器未运行", "The container is not running"), tr("启动", "Start")) {
			s.startContainer(x)
		}
		return
	}
	path := s.filePath[x.ID]
	if path == "" {
		path = "/"
	}
	if want := x.ID + ":" + path; s.detail.filesFor != want {
		s.detail.filesFor, s.detail.files, s.detail.filesErr = want, nil, ""
		key := s.detail.key
		s.run(10*time.Second, func(ctx context.Context) func() {
			out, err := s.engine.exec(ctx, x.ID, []string{"ls", "-1Ap", "--", path})
			return func() {
				if s.detail.key != key || s.detail.filesFor != want {
					return
				}
				if err != nil {
					s.detail.filesErr = err.Error()
				} else if out = strings.TrimSpace(out); out != "" {
					s.detail.files = strings.Split(out, "\n")
				}
			}
		})
	}
	ui.Row(c.Key("crumbs")).FillWidth().Height(28).Shrink(0).Padding(0, 12, 0, 8).Gap(6).AlignItems(ui.Center).
		BorderWidth(0, 0, 1, 0).BorderColor(p.line).Children(func() {
		if iconButton(c.Key("up"), p, "up", tr("上一级", "Up"), path == "/").Clicked() {
			parent := strings.TrimSuffix(path, "/")
			s.filePath[x.ID] = parent[:strings.LastIndex(parent, "/")+1]
		}
		label(c, path, p.text)
	})
	if s.detail.filesErr != "" {
		note(c, p, s.detail.filesErr)
		return
	}
	entries := s.detail.files
	if len(entries) == 0 {
		note(c, p, tr("空目录", "Empty directory"))
		return
	}
	ui.List(c.Key("files"), s.list("files"), len(entries)).FillWidth().Grow(1).MinHeight(0).Rows(func(r ui.ListRow) {
		name := entries[r.Index]
		rc := r.Context
		if !strings.HasSuffix(name, "/") {
			ui.Row(rc).FillWidth().Height(26).PaddingX(14).Gap(10).AlignItems(ui.Center).BorderWidth(0, 0, 1, 0).BorderColor(p.line).Children(func() {
				ui.Icon(rc, icons["file"]).Size(14, 14).Shrink(0).TextColor(p.muted)
				label(rc, name, p.text)
			})
			return
		}
		b := clickable(rc).Label(tr("打开 ", "Open ")+name).FillWidth().Height(26).PaddingX(14).Gap(10).
			AlignItems(ui.Center).Justify(ui.Start).BorderWidth(0, 0, 1, 0).BorderColor(p.line)
		if b.Hovered() {
			b.Background(p.hover)
		}
		b.Children(func() {
			ui.Icon(rc, icons["folder"]).Size(14, 14).Shrink(0).TextColor(p.accent)
			label(rc, strings.TrimSuffix(name, "/"), p.text)
		})
		if b.Clicked() {
			s.filePath[x.ID] = path + name
		}
	})
}
