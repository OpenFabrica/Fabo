package main

import (
	"cmp"
	"context"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/egoist/mygo/ui"
)

// buildsUI is the Builds page: BuildKit's history, the picked build's
// detail and how its steps show.
type buildsUI struct {
	// visited marks that this visit's first read has started; read that a
	// list has arrived at least once.
	visited, read, loading bool
	last                   time.Time
	err                    string
	items                  []buildRecord
	// picked is set once the newest build was opened for the first list.
	picked bool

	// The panel's build, by key, with its steps, and the image it made.
	shown                      string
	detail                     *buildDetail
	detailErr                  string
	detailLoading, detailStale bool
	layers                     *buildLayers
	layersErr                  string
	layersLoading, layersAsked bool
	scanAsked                  map[string]bool
	open                       map[string]bool
	steps                      int
	internal                   bool
	platform                   int
	removing                   bool
}

// buildPoll is how often the open page reads the history, and how often
// while a build runs.
const (
	buildPoll     = 15 * time.Second
	buildPollBusy = 2 * time.Second
)

// buildFilters are the list's state tabs, as upstream's: All, then each
// kind in the order a build goes through them.
var buildFilters = []struct{ kind, zh, en string }{
	{"", "全部", "All"}, {"run", "构建中", "Building"}, {"ok", "已完成", "Completed"}, {"fail", "失败", "Error"}, {"off", "已取消", "Canceled"},
}

// buildHistory is the history as this window reads it: through the
// docker CLI, against this engine's context and the containers it has.
func (s *desktop) buildHistory() buildHistory {
	if s.buildsFake != nil {
		return *s.buildsFake
	}
	have := map[string]bool{}
	for _, x := range s.containers {
		have[x.Name] = true
	}
	return buildHistory{run: runDockerCLI, have: have, context: s.contextName}
}

// refreshBuilds reads the history while the page is open: once at the
// start of each visit, then every 15 s, or every 2 s while a build runs.
// The later reads wait for the VM to be awake, so the page does not keep
// waking a sleeping one; reads never overlap.
func (s *desktop) refreshBuilds() {
	b := &s.bl
	if b.loading || !s.connected || s.info.FaboStopped {
		return
	}
	running := slices.ContainsFunc(b.items, func(x buildRecord) bool { return buildKind(x.Status) == "run" })
	since := time.Since(b.last)
	if b.visited && !(s.info.awake() && (since >= buildPoll || running && since >= buildPollBusy)) {
		return
	}
	b.visited, b.loading = true, true
	h := s.buildHistory()
	s.run(time.Minute, func(ctx context.Context) func() {
		items, err := h.list(ctx)
		return func() {
			b.loading, b.last = false, time.Now()
			if err != nil {
				b.err = err.Error()
				return
			}
			b.err, b.read, b.items = "", true, items
			s.followBuild()
		}
	})
	if s.engine.http == nil {
		b.loading = false
	}
}

// followBuild keeps the panel on a listed build: the newest opens with the
// first list (as upstream's view does), the newest again when the open one
// left the history, and the open one is read again while it runs or when
// its state changed.
func (s *desktop) followBuild() {
	b := &s.bl
	key := s.selected[buildsPage]
	rec, found := s.buildRecord(key)
	switch {
	case key == "" && !b.picked && len(b.items) > 0, key != "" && !found && len(b.items) > 0:
		b.picked = true
		s.selected[buildsPage] = b.items[0].key()
		return
	case key != "" && !found:
		// The answer may land after navigation: only this page's panel
		// closes, and the page is the one keyed, not the current one.
		s.selected[buildsPage] = ""
		return
	}
	b.picked = true
	if found && b.shown == key && b.detail != nil && (buildKind(rec.Status) == "run" || rec.Status != b.detail.listStatus) {
		b.detailStale = true
		// A build that finished since may have made an image.
		if rec.Status != b.detail.listStatus {
			b.layersAsked = false
		}
	}
}

// buildRecord finds a listed build by key.
func (s *desktop) buildRecord(key string) (buildRecord, bool) {
	for _, x := range s.bl.items {
		if x.key() == key {
			return x, true
		}
	}
	return buildRecord{}, false
}

// showBuild makes the panel's build the selected one, reading its detail
// when it is new or stale, and its image when the Layers or Security tab
// wants it.
func (s *desktop) showBuild(rec buildRecord) {
	b := &s.bl
	key := rec.key()
	if b.shown != key {
		b.shown, b.detail, b.detailErr, b.layers, b.layersErr, b.layersAsked = key, nil, "", nil, "", false
		b.open, b.platform = map[string]bool{}, 0
		b.detailStale = true
	}
	h := s.buildHistory()
	if b.detailStale && !b.detailLoading {
		b.detailStale, b.detailLoading = false, true
		s.run(time.Minute, func(ctx context.Context) func() {
			d, err := h.detail(ctx, rec.Builder, rec.ID)
			return func() {
				b.detailLoading = false
				if b.shown != key {
					return
				}
				if err != nil {
					b.detailErr = err.Error()
					return
				}
				d.listStatus = rec.Status
				b.detail, b.detailErr = d, ""
			}
		})
		if s.engine.http == nil {
			b.detailLoading = false
		}
	}
	tab := s.tab[buildsPage]
	if (tab == 1 || tab == 2) && !b.layersAsked && !b.layersLoading {
		b.layersAsked, b.layersLoading = true, true
		s.run(time.Minute, func(ctx context.Context) func() {
			l, err := h.layers(ctx, rec.Builder, rec.ID)
			return func() {
				b.layersLoading = false
				if b.shown != key {
					return
				}
				if err != nil {
					b.layersErr = err.Error()
					return
				}
				b.layers, b.layersErr = l, ""
			}
		})
		if s.engine.http == nil {
			b.layersLoading = false
		}
	}
}

// removeBuilds takes builds out of the history, a builder at a time, then
// reads the list again.
func (s *desktop) removeBuilds(list []buildRecord, done string) {
	b := &s.bl
	b.removing = true
	h := s.buildHistory()
	byBuilder := map[string][]string{}
	var order []string
	for _, x := range list {
		if byBuilder[x.Builder] == nil {
			order = append(order, x.Builder)
		}
		byBuilder[x.Builder] = append(byBuilder[x.Builder], x.ID)
	}
	s.run(time.Minute, func(ctx context.Context) func() {
		var err error
		for _, builder := range order {
			if err = h.remove(ctx, builder, byBuilder[builder]); err != nil {
				break
			}
		}
		return func() {
			b.removing = false
			if err != nil {
				s.notices = append(s.notices, tr("无法删除构建：", "Couldn't remove the build: ")+err.Error())
			} else {
				s.notices = append(s.notices, done)
				b.items = slices.DeleteFunc(b.items, func(x buildRecord) bool { return slices.Contains(byBuilder[x.Builder], x.ID) })
			}
			b.visited = false
			s.refreshBuilds()
		}
	})
	if s.engine.http == nil {
		b.removing = false
	}
}

// buildDuration words a span as upstream does: tenths under a second, then
// seconds, then minutes and seconds.
func buildDuration(d time.Duration) string {
	switch {
	case d < 0:
		return ""
	case d < time.Second:
		return fmt.Sprintf("%.1fs", d.Seconds())
	}
	sec := int64(d.Round(time.Second) / time.Second)
	if sec < 60 {
		return fmt.Sprintf("%ds", sec)
	}
	return fmt.Sprintf("%dm %ds", sec/60, sec%60)
}

// buildSpan is how long something took, or has taken so far when it has
// not ended; zero start means unknown.
func buildSpan(start, end time.Time) time.Duration {
	switch {
	case start.IsZero():
		return -1
	case end.IsZero():
		return time.Since(start)
	}
	return end.Sub(start)
}

// buildWords names a build's state.
func buildWords(kind string) string {
	switch kind {
	case "ok":
		return tr("已完成", "Completed")
	case "fail":
		return tr("失败", "Error")
	case "run":
		return tr("构建中", "Building")
	}
	return tr("已取消", "Canceled")
}

// buildInk is the color of a build's or a step's state.
func buildInk(p palette, kind string) ui.Color {
	switch kind {
	case "ok":
		return p.ok
	case "fail":
		return p.err
	case "run":
		return p.accent
	}
	return p.muted
}

// buildIcon is the mark of a build's or a step's state: a spinner while it
// runs, a check, a cross, a bolt for a cached step, else a dash.
func buildIcon(c *ui.Context, p palette, kind string) {
	ui.Box(c).Size(16, 16).Shrink(0).Center().Children(func() {
		icon := map[string]string{"ok": "check", "fail": "x", "cached": "bolt"}[kind]
		switch {
		case kind == "run":
			ui.Spinner(c).Size(12, 12)
		case icon != "":
			ui.Icon(c, icons[icon]).Size(14, 14).TextColor(buildInk(p, kind))
		default:
			ui.Icon(c, icons["minus"]).Size(14, 14).TextColor(p.muted)
		}
	})
}

// buildPill is a build's state as a tinted chip.
func buildPill(c *ui.Context, p palette, kind string) {
	ink := buildInk(p, kind)
	ui.Text(c, buildWords(kind)).FontSize(11).FontWeight(600).TextColor(ink).Padding(1, 8).Radius(10).Background(ink.Alpha(0.12)).Shrink(0)
}

// emptyNote is an empty page with a line under its title.
func emptyNote(c *ui.Context, p palette, icon, title, body string) {
	ui.Column(c).Fill().Grow(1).MinHeight(120).Center().Gap(10).Children(func() {
		ui.Box(c).Size(52, 52).Radius(14).Background(p.soft).Center().Children(func() {
			ui.Icon(c, icons[icon]).Size(24, 24).TextColor(p.muted)
		})
		ui.Text(c, title).FontSize(13.5).FontWeight(500).TextColor(p.text)
		if body != "" {
			ui.Text(c, body).FontSize(12).MaxWidth(420).TextAlign(ui.Center).LineHeight(1.45).TextColor(p.muted)
		}
	})
}

// buildsView is the Builds page: the history, newest first, filtered by
// state and the search, with the picked build's panel beside it.
func (s *desktop) buildsView(c *ui.Context, p palette) {
	b := &s.bl
	if !b.read {
		if b.err != "" {
			emptyNote(c, p, "hammer", tr("读不到构建历史", "Couldn't read the build history"), b.err)
		} else {
			emptyNote(c, p, "hammer", tr("正在读取构建历史…", "Reading build history…"), tr("构建器休眠时会先启动它。", "BuildKit starts if it's asleep."))
		}
		return
	}
	if len(b.items) == 0 {
		emptyNote(c, p, "hammer", tr("还没有构建", "No builds yet"),
			tr("用 docker build 构建后会显示在这里，带每一步的耗时、缓存和输出。", "Builds you run with docker build show up here, with each step's time, cache and output."))
		return
	}
	query := strings.ToLower(strings.TrimSpace(s.search))
	filter := min(max(0, s.filter[buildsPage]), len(buildFilters)-1)
	counts := make([]int, len(buildFilters))
	var shown, failed []buildRecord
	for _, x := range b.items {
		kind := buildKind(x.Status)
		if kind == "fail" {
			failed = append(failed, x)
		}
		if query != "" && !strings.Contains(strings.ToLower(x.Name), query) && !strings.Contains(strings.ToLower(x.ID), query) {
			continue
		}
		counts[0]++
		for i, f := range buildFilters[1:] {
			if f.kind == kind {
				counts[i+1]++
			}
		}
		if filter == 0 || buildFilters[filter].kind == kind {
			shown = append(shown, x)
		}
	}
	toolbar(c, func() {
		labels := make([]string, len(buildFilters))
		for i, f := range buildFilters {
			labels[i] = tr(f.zh, f.en)
		}
		selected := filter
		if filters(c.Key("build-filter"), p, tr("构建状态", "Build state"), &selected, labels, counts) {
			s.filter[buildsPage] = selected
		}
		ui.Box(c).Grow(1)
		if len(failed) > 0 {
			text := fmt.Sprintf(tr("清理 %d 个失败的构建", "Clear %d failed"), len(failed))
			if button(c.Key("clear-failed"), p, text, "trash", plainButton, b.removing) {
				s.pending = &confirmation{
					title: fmt.Sprintf(tr("清理 %d 个失败的构建？", "Clear %d failed builds?"), len(failed)),
					body:  fmt.Sprintf(tr("从构建历史中删除这 %d 个失败的构建。", "Removes the %d failed builds from the history."), len(failed)),
					ok:    tr("清理", "Clear"),
					run:   func() { s.removeBuilds(failed, tr("已清理失败的构建", "Failed builds cleared")) },
				}
			}
		}
	})
	cols := narrowColumns([]column{{tr("名称", "Name"), 0, 1, false}, {tr("步骤", "Steps"), 110, 0, false},
		{tr("耗时", "Duration"), 70, 0, true}, {tr("开始", "Started"), 90, 0, false}}, s.narrow(), 1, 3)
	const actionsWidth = 28
	if !s.dockMax {
		tableHeader(c, p, cols, actionsWidth, nil)
		if len(shown) == 0 {
			empty(c, p, "hammer", tr("没有匹配的构建", "No build matches"), "")
		} else {
			ui.List(c.Key("rows"), s.list(buildsPage), len(shown)).FillWidth().Grow(1).MinHeight(0).
				ItemKey(func(i int) any { return shown[i].key() }).
				Rows(func(r ui.ListRow) { s.buildRow(r.Context, p, shown[r.Index], cols, actionsWidth) })
		}
	}
	if rec, ok := s.buildRecord(s.selected[buildsPage]); ok {
		s.showBuild(rec)
		s.buildDock(c, p, rec)
	}
}

// buildRow is one build in the list.
func (s *desktop) buildRow(c *ui.Context, p palette, x buildRecord, cols []column, actionsWidth float32) {
	kind := buildKind(x.Status)
	s.tableRow(c, p, x.key(), x.title(), actionsWidth, nil, func() {
		cell(c, 0, 1).Gap(10).Children(func() {
			buildIcon(c, p, kind)
			name := label(c, x.title(), p.text).FontWeight(500)
			if x.Name == "" {
				name.Font(mono).FontSize(11.5).TextColor(p.sub)
			}
		})
		if !s.narrow() {
			cell(c, cols[1].width, 0).Children(func() {
				steps := fmt.Sprintf("%d/%d", x.Done, x.Total)
				if x.Cached > 0 {
					steps += fmt.Sprintf(tr(" · %d 缓存", " · %d cached"), x.Cached)
				}
				label(c, steps, p.muted).FontFeatures("tnum")
			})
		}
		durCol := cols[len(cols)-1]
		if !s.narrow() {
			durCol = cols[2]
		}
		cell(c, durCol.width, 0).Children(func() {
			label(c, buildDuration(buildSpan(x.Started, x.Completed)), p.text).FontFeatures("tnum").FillWidth().TextAlign(ui.End)
		})
		if !s.narrow() {
			cell(c, cols[3].width, 0).Children(func() { label(c, since(s.now, x.Started.Unix()), p.muted) })
		}
	}, func() {
		if iconButton(c.Key("delete"), p, "trash", tr("移除 ", "Remove ")+x.title(), s.bl.removing || kind == "run").Clicked() {
			s.confirmRemoveBuild(x)
		}
	})
}

// confirmRemoveBuild asks before taking a build out of the history.
func (s *desktop) confirmRemoveBuild(x buildRecord) {
	s.pending = &confirmation{
		title: fmt.Sprintf(tr("从构建历史中删除 %s？", "Remove %s from the history?"), x.title()),
		body:  tr("只删除这条记录，构建缓存和产出的镜像都保留。", "Only the record goes; the build cache and the image it made stay."),
		ok:    tr("删除", "Delete"),
		run:   func() { s.removeBuilds([]buildRecord{x}, fmt.Sprintf(tr("已删除 %s", "Removed %s"), x.title())) },
	}
}

// buildDock is the picked build's panel: its state, time and steps under
// the title, then the Steps, Layers, Security and Info tabs.
func (s *desktop) buildDock(c *ui.Context, p palette, rec buildRecord) {
	b := &s.bl
	d := b.detail
	kind := buildKind(rec.Status)
	if d != nil {
		kind = d.kind()
	}
	steps := tr("步骤", "Steps")
	if d != nil {
		// The count is the Dockerfile's steps, which the tab shows first.
		n := 0
		for _, st := range d.Steps {
			if !st.internal() {
				n++
			}
		}
		if n > 0 {
			steps += " " + strconv.Itoa(n)
		}
	}
	s.dock(c, p, rec.title(), func() { buildPill(c, p, kind) },
		[]string{steps, tr("分层", "Layers"), tr("安全扫描", "Security"), tr("信息", "Info")},
		func() {
			meta := func(icon, text string, ink ui.Color, tip string) {
				ui.Row(c).Gap(5).AlignItems(ui.Center).Shrink(0).Tooltip(tip).Children(func() {
					ui.Icon(c, icons[icon]).Size(13, 13).TextColor(ink)
					ui.Text(c, text).FontSize(12).FontFeatures("tnum").TextColor(ink)
				})
			}
			src := rec
			if d != nil {
				src = d.buildRecord
			}
			meta("clock", cmp.Or(buildDuration(buildSpan(src.Started, src.Completed)), "—"), p.muted, tr("耗时", "Duration"))
			n := fmt.Sprintf("%d/%d", src.Done, src.Total)
			if src.Cached > 0 {
				n += fmt.Sprintf(tr("（%d 缓存）", " (%d cached)"), src.Cached)
			}
			meta("stack", n, p.muted, tr("步骤", "Steps"))
			if d != nil && len(d.Warnings) > 0 {
				meta("alert", strconv.Itoa(len(d.Warnings)), p.warn, tr("警告", "Warnings"))
			}
			ui.Box(c).Grow(1)
			if iconButton(c.Key("copy-build"), p, "copy", tr("复制构建 ID", "Copy build ID"), false).Clicked() {
				c.WriteClipboard(rec.ID)
				s.notices = append(s.notices, tr("已复制构建 ID", "Build ID copied"))
			}
			if iconButton(c.Key("remove-build"), p, "trash", tr("删除构建记录", "Remove from the history"), b.removing || kind == "run").Clicked() {
				s.confirmRemoveBuild(rec)
			}
		},
		func(tab int) {
			switch tab {
			case 0:
				s.buildStepsTab(c, p, d)
			case 1:
				s.buildLayersTab(c, p)
			case 2:
				s.buildSecurityTab(c, p)
			case 3:
				s.buildInfoTab(c, p, rec, d)
			}
		})
}

// stepFilters are the Steps tab's choices: every step, the ones that
// ran, the cached ones and the failed ones.
var stepFilters = []struct{ zh, en string }{{"全部", "All"}, {"已执行", "Ran"}, {"缓存", "Cached"}, {"失败", "Error"}}

// stepPrefix splits a step's "[2/4]" from the rest of its name.
var stepPrefix = regexp.MustCompile(`(?s)^(\[[^\]]*\])\s*(.*)$`)

// pinnedDigest shortens an image pinned by digest, as
// alpine:3.20@sha256:be2656061b53…; the tooltip has it all.
var pinnedDigest = regexp.MustCompile(`@sha256:([0-9a-f]{12})[0-9a-f]{52}`)

// buildStepsTab shows why a build failed, its warnings and its steps, each
// with its time against the slowest, or "cached", and its output on a
// click.
func (s *desktop) buildStepsTab(c *ui.Context, p palette, d *buildDetail) {
	b := &s.bl
	if d == nil {
		if b.detailErr != "" {
			note(c, p, b.detailErr)
		} else {
			note(c, p, tr("正在读取步骤…", "Loading the steps…"))
		}
		return
	}
	var steps []buildStep
	for _, st := range d.Steps {
		state := st.state()
		switch {
		case !b.internal && st.internal():
		case b.steps == 1 && (state == "cached" || state == "skip"):
		case b.steps == 2 && state != "cached":
		case b.steps == 3 && state != "fail":
		default:
			steps = append(steps, st)
		}
	}
	var slowest time.Duration = 1
	openable := 0
	allOpen := true
	for _, st := range steps {
		if !st.Cached {
			slowest = max(slowest, buildSpan(st.Started, st.Completed))
		}
		if st.Log != "" || st.Error != "" {
			openable++
			allOpen = allOpen && b.open[st.Name]
		}
	}
	allOpen = allOpen && openable > 0
	ui.Scroll(c.Key("build-steps")).Fill().Grow(1).MinHeight(0).Children(func() {
		ui.Column(c).FillWidth().Padding(12, 16, 16).Gap(10).Children(func() {
			if d.Error != nil && d.Error.Message != "" {
				buildFailureBox(c, p, d)
			}
			for i, w := range d.Warnings {
				buildWarningRow(c.Key("warning-"+strconv.Itoa(i)), p, w)
			}
			ui.Row(c.Key("step-bar")).FillWidth().Gap(12).AlignItems(ui.Center).Children(func() {
				// A drop-down, as upstream's, so the bar fits a narrow panel.
				labels := make([]string, len(stepFilters))
				for i, f := range stepFilters {
					labels[i] = tr(f.zh, f.en)
				}
				chosen := labels[min(max(0, b.steps), len(labels)-1)]
				if ui.Select(c.Key("step-filter"), &chosen, labels).Label(tr("显示哪些步骤", "Which steps")).Height(30).MinWidth(96).Shrink(0).
					Cursor(ui.CursorPointer).Changed() {
					b.steps = max(0, slices.Index(labels, chosen))
				}
				// The box and its words are two controls side by side, so
				// that no button holds another; either one toggles.
				ui.Row(c.Key("internal")).Gap(6).AlignItems(ui.Center).Shrink(0).Children(func() {
					state := 0
					if b.internal {
						state = 2
					}
					toggled := checkbox(c.Key("internal-box"), p, tr("显示内部步骤", "Show internal steps"), state)
					words := clickable(c.Key("internal-words")).Label(tr("内部步骤", "Internal steps")).Children(func() {
						ui.Text(c, tr("内部步骤", "Internal steps")).FontSize(12.5).TextColor(p.text)
					})
					if toggled || words.Clicked() {
						b.internal = !b.internal
					}
				})
				ui.Box(c).Grow(1)
				text, icon := tr("全部展开", "Expand all"), "expand"
				if allOpen {
					text, icon = tr("全部收起", "Collapse all"), "collapse"
				}
				if iconButton(c.Key("open-all"), p, icon, text, openable == 0).Tooltip(text).Clicked() {
					for _, st := range steps {
						if st.Log != "" || st.Error != "" {
							b.open[st.Name] = !allOpen
						}
					}
				}
			})
			if len(steps) == 0 {
				note(c, p, tr("这里没有步骤", "No steps here"))
				return
			}
			ui.Column(c.Key("steps")).FillWidth().Radius(8).Border(1, p.line).Clip().Children(func() {
				for i, st := range steps {
					s.buildStepRow(c.Key(i), p, st, slowest, i == len(steps)-1)
				}
			})
		})
	})
}

// buildStepRow is one step: its state, its name and its time, opening onto
// its output.
func (s *desktop) buildStepRow(c *ui.Context, p palette, st buildStep, slowest time.Duration, last bool) {
	b := &s.bl
	state := st.state()
	has := st.Log != "" || st.Error != ""
	open := has && b.open[st.Name]
	ui.Column(c).FillWidth().Children(func() {
		// A step with output is a button that opens it; one without is a
		// plain row, neither clickable nor dimmed.
		var row ui.Element
		if has {
			row = clickable(c.Key("step")).Label(st.Name)
			if row.Hovered() {
				row.Background(p.hover)
			}
		} else {
			row = ui.Row(c.Key("step"))
		}
		row.Tooltip(st.Name).FillWidth().Height(30).PaddingX(12).Gap(10).AlignItems(ui.Center).Justify(ui.Start)
		if !last || open {
			row.BorderWidth(0, 0, 1, 0).BorderColor(p.line)
		}
		row.Children(func() {
			buildIcon(c, p, state)
			ink := p.text
			if state == "cached" || state == "skip" {
				ink = p.muted
			}
			ui.Row(c).Grow(1).Width(0).MinWidth(0).Gap(6).AlignItems(ui.Center).Children(func() {
				name := pinnedDigest.ReplaceAllString(st.Name, "@sha256:$1…")
				if m := stepPrefix.FindStringSubmatch(name); m != nil {
					ui.Text(c, m[1]).Font(mono).FontSize(11.5).TextColor(p.muted).Shrink(0)
					name = m[2]
				}
				ui.Text(c, strings.Join(strings.Fields(name), " ")).Font(mono).FontSize(11.5).TextColor(ink).SingleLine().MinWidth(0)
			})
			switch state {
			case "cached":
				ui.Text(c, tr("缓存", "cached")).FontSize(11.5).TextColor(p.muted).Shrink(0)
			case "skip":
				ui.Text(c, tr("跳过", "skipped")).FontSize(11.5).TextColor(p.muted).Shrink(0)
			default:
				took := buildSpan(st.Started, st.Completed)
				ui.Column(c).Width(110).Shrink(0).Gap(4).AlignItems(ui.End).Children(func() {
					ui.Text(c, buildDuration(took)).Font(mono).FontSize(11).FontFeatures("tnum").TextColor(p.text)
					ink := p.accent
					if state == "fail" {
						ink = p.err
					}
					meter(c, p, max(0.02, float64(took)/float64(slowest)), 3, ink)
				})
			}
		})
		if row.Clicked() && has {
			b.open[st.Name] = !open
		}
		if open {
			text := strings.TrimSpace(st.Log)
			if st.Error != "" {
				text = strings.TrimSpace(text + "\n" + st.Error)
			}
			log := ui.Scroll(c.Key("log")).FillWidth().MaxHeight(320).Background(p.soft)
			if !last {
				log.BorderWidth(0, 0, 1, 0).BorderColor(p.line)
			}
			log.Children(func() {
				ui.Text(c, text).Font(mono).FontSize(11).LineHeight(1.5).TextColor(p.sub).Selectable().FillWidth().Padding(8, 12, 10, 38)
			})
		}
	})
}

// buildFailureBox says why the build failed (or that it was canceled),
// with the Dockerfile lines around the failing one.
func buildFailureBox(c *ui.Context, p palette, d *buildDetail) {
	off := d.kind() == "off"
	ink, title := p.err, cmp.Or(d.Error.Step, tr("构建失败", "The build failed"))
	if off {
		ink, title = p.muted, tr("构建已取消", "The build was canceled")
	}
	ui.Column(c.Key("failure")).FillWidth().Padding(10, 12).Gap(6).Radius(8).Border(1, ink.Alpha(0.35)).Background(ink.Alpha(0.06)).Children(func() {
		ui.Row(c).FillWidth().Gap(7).AlignItems(ui.Center).Children(func() {
			ui.Icon(c, icons["x"]).Size(14, 14).Shrink(0).TextColor(ink)
			ui.Text(c, title).Font(mono).FontSize(12).FontWeight(600).TextColor(ink).MinWidth(0)
		})
		ui.Text(c, d.Error.Message).FontSize(12).LineHeight(1.45).TextColor(p.text).Selectable()
		if src := strings.TrimSpace(d.Error.Source); src != "" {
			ui.Text(c, src).Font(mono).FontSize(11).LineHeight(1.5).TextColor(p.sub).Selectable()
		}
	})
}

// buildWarningRow is one of the build checks' warnings, with its line and
// a link to Docker's page about it.
func buildWarningRow(c *ui.Context, p palette, w buildWarning) {
	ui.Row(c).FillWidth().Padding(8, 12).Gap(8).AlignItems(ui.Start).Radius(8).Border(1, p.warn.Alpha(0.3)).Background(p.warn.Alpha(0.07)).Children(func() {
		ui.Icon(c, icons["alert"]).Size(14, 14).Shrink(0).TextColor(p.warn)
		ui.Column(c).Grow(1).Width(0).MinWidth(0).Gap(2).Children(func() {
			// BuildKit's text already ends in "(line N)"; it is said once,
			// in the window's language.
			text := w.Text
			if w.Line > 0 {
				text = strings.TrimSuffix(text, fmt.Sprintf(" (line %d)", w.Line)) + fmt.Sprintf(tr("（第 %d 行）", " (line %d)"), w.Line)
			}
			ui.Text(c, text).FontSize(12).TextColor(p.text).Selectable()
			if w.Detail != "" {
				ui.Text(c, w.Detail).FontSize(11.5).TextColor(p.muted)
			}
		})
		if strings.HasPrefix(w.URL, "https://docs.docker.com/") && openLink(c, p, "out", tr("文档", "Docs"), w.URL) {
			c.OpenURL(w.URL)
		}
	})
}

// buildImageShown is the platform's image the Layers and Security tabs
// show, or nil before the image is read or when the build made none.
func (s *desktop) buildImageShown() *buildImage {
	l := s.bl.layers
	if l == nil || len(l.Images) == 0 {
		return nil
	}
	return &l.Images[min(max(0, s.bl.platform), len(l.Images)-1)]
}

// buildLayersNote is what the Layers and Security tabs say until there is
// an image to show; it reports whether it said something.
func (s *desktop) buildLayersNote(c *ui.Context, p palette, none string) bool {
	b := &s.bl
	switch {
	case b.layersErr != "":
		note(c, p, b.layersErr)
	case b.layers == nil:
		note(c, p, tr("正在读取镜像…", "Reading the image…"))
	case len(b.layers.Images) == 0:
		note(c, p, none)
	default:
		return false
	}
	return true
}

// buildLayersTab shows the image the build made: its size and layers,
// this build's work first, then what came from the base image, and how it
// runs.
func (s *desktop) buildLayersTab(c *ui.Context, p palette) {
	if s.buildLayersNote(c, p, tr("这次构建没有产出镜像：导出前就失败了，或者只输出了缓存。", "This build made no image: it failed before exporting, or its output was the cache only.")) {
		return
	}
	b := &s.bl
	img := s.buildImageShown()
	var own, base []int
	var ownSize, baseSize int64
	var biggest int64 = 1
	real := 0
	for i, l := range img.Layers {
		if l.Base {
			base = append(base, i)
			baseSize += l.Size
		} else {
			own = append(own, i)
			ownSize += l.Size
		}
		if !l.Empty {
			real++
			biggest = max(biggest, l.Size)
		}
	}
	ui.Scroll(c.Key("build-layers")).Fill().Grow(1).MinHeight(0).Children(func() {
		ui.Column(c).FillWidth().Padding(12, 16, 16).Gap(12).Children(func() {
			ui.Row(c.Key("sum")).FillWidth().Gap(16).AlignItems(ui.Center).Children(func() {
				fact := func(big, small string) {
					ui.Row(c).Gap(4).AlignItems(ui.End).Shrink(0).Children(func() {
						ui.Text(c, big).FontSize(13).FontWeight(600).FontFeatures("tnum").TextColor(p.text)
						ui.Text(c, small).FontSize(12).TextColor(p.muted)
					})
				}
				fact(displaySize(img.Size), tr("压缩后", "compressed"))
				fact(strconv.Itoa(real), tr("层", "layers"))
				fact(displaySize(ownSize), tr("本次构建添加", "added by this build"))
				ui.Box(c).Grow(1)
				if len(b.layers.Images) > 1 {
					names := make([]string, len(b.layers.Images))
					for i, im := range b.layers.Images {
						names[i] = im.Platform
					}
					filters(c.Key("platform"), p, tr("平台", "Platform"), &b.platform, names, nil)
				} else {
					ui.Text(c, img.Platform).Font(mono).FontSize(11.5).TextColor(p.muted).Shrink(0)
				}
			})
			if len(b.layers.Bases) > 0 {
				ui.Text(c, "FROM "+strings.Join(b.layers.Bases, ", ")).Font(mono).FontSize(11.5).TextColor(p.muted).Tooltip(tr("基础镜像", "Base image")).
					SingleLine().MinWidth(0)
			}
			group := func(key, title string, lines []int, size int64, faint bool) {
				if len(lines) == 0 {
					return
				}
				layers := 0
				for _, i := range lines {
					if !img.Layers[i].Empty {
						layers++
					}
				}
				buildGroup(c.Key(key), p, title, fmt.Sprintf(tr("%d 层 · %s", "%d layers · %s"), layers, displaySize(size)), func() {
					for _, i := range lines {
						buildLayerRow(c.Key(i), p, i, img.Layers[i], biggest, faint)
					}
				})
			}
			group("own", tr("本次构建添加", "Added by this build"), own, ownSize, false)
			group("base", tr("来自基础镜像", "From the base image"), base, baseSize, true)
			cfg := img.Config
			var env []string
			for _, e := range cfg.Env {
				if !strings.HasPrefix(e, "PATH=") {
					env = append(env, e)
				}
			}
			var labels []string
			for _, k := range sortedKeys(cfg.Labels) {
				labels = append(labels, k+"="+cfg.Labels[k])
			}
			buildGroup(c.Key("runs"), p, tr("运行方式", "How it runs"), tr("镜像的设置", "the image's settings"), func() {
				for i, f := range []field{
					{tr("运行", "Runs"), strings.Join(append(slices.Clone(cfg.Entrypoint), cfg.Cmd...), " ")},
					{tr("工作目录", "Working dir"), cfg.WorkingDir}, {tr("用户", "User"), cmp.Or(cfg.User, "root")},
					{tr("端口", "Ports"), strings.Join(cfg.Ports, ", ")}, {tr("卷", "Volumes"), strings.Join(cfg.Volumes, ", ")},
					{tr("健康检查", "Health check"), strings.Join(cfg.Healthcheck, " ")}, {tr("停止信号", "Stop signal"), cfg.StopSignal},
					{tr("环境变量", "Environment"), strings.Join(env, "\n")}, {tr("标签", "Labels"), strings.Join(labels, "\n")},
				} {
					if f.value == "" {
						continue
					}
					ui.Row(c.Key(i)).FillWidth().Padding(6, 12).Gap(12).AlignItems(ui.Start).BorderWidth(1, 0, 0, 0).BorderColor(p.line).Children(func() {
						ui.Text(c, f.label).Width(96).Shrink(0).FontSize(12).TextColor(p.muted)
						ui.Text(c, f.value).Width(0).Grow(1).MinWidth(0).Font(mono).FontSize(11.5).TextColor(p.text).Selectable()
					})
				}
			})
		})
	})
}

// buildGroup is a bordered block of rows under a title and a note.
func buildGroup(c *ui.Context, p palette, title, note string, rows func()) {
	ui.Column(c).FillWidth().Radius(8).Border(1, p.line).Clip().Children(func() {
		ui.Row(c).FillWidth().Padding(7, 12).Gap(12).AlignItems(ui.Center).Children(func() {
			ui.Text(c, title).FontSize(12.5).FontWeight(600).TextColor(p.text).Grow(1)
			ui.Text(c, note).FontSize(12).TextColor(p.muted)
		})
		rows()
	})
}

// buildLayerRow is one history line: its number, the instruction and the
// layer's size against the biggest, or "settings" for a line that adds
// no layer.
func buildLayerRow(c *ui.Context, p palette, i int, l buildLayer, biggest int64, faint bool) {
	tip := l.By + "\n" + l.Digest
	if l.Empty {
		tip = l.By + "\n" + tr("没有层：这一行只改镜像的设置", "No layer: this line only changes the image's settings")
	}
	ink := p.text
	if l.Empty || faint {
		ink = p.muted
	}
	ui.Row(c).FillWidth().Height(28).PaddingX(12).Gap(10).AlignItems(ui.Center).BorderWidth(1, 0, 0, 0).BorderColor(p.line).Tooltip(tip).Children(func() {
		ui.Text(c, strconv.Itoa(i+1)).Width(22).Shrink(0).Font(mono).FontSize(11).TextColor(p.muted).TextAlign(ui.End)
		ui.Text(c, cmp.Or(strings.Join(strings.Fields(l.By), " "), tr("（无描述）", "(no description)"))).Font(mono).FontSize(11.5).TextColor(ink).
			SingleLine().Grow(1).Width(0).MinWidth(0)
		if l.Empty {
			ui.Text(c, tr("设置", "settings")).Width(110).Shrink(0).FontSize(11.5).TextColor(p.muted).TextAlign(ui.End)
			return
		}
		ui.Column(c).Width(110).Shrink(0).Gap(4).AlignItems(ui.End).Children(func() {
			ui.Text(c, displaySize(l.Size)).Font(mono).FontSize(11).FontFeatures("tnum").TextColor(p.text)
			bar := p.accent
			if faint {
				bar = p.muted
			}
			meter(c, p, max(0.02, float64(l.Size)/float64(biggest)), 3, bar)
		})
	})
}

// buildSecurityTab scans the image the build made, once it is in Fabo:
// the same scan as the image's own page.
func (s *desktop) buildSecurityTab(c *ui.Context, p palette) {
	if s.buildLayersNote(c, p, tr("这次构建没有产出可扫描的镜像：导出前就失败了，或者只输出了缓存。", "This build made no image to scan: it failed before exporting, or its output was the cache only.")) {
		return
	}
	shown := s.buildImageShown()
	img, ok := s.image(shown.ConfigDigest)
	if !ok {
		emptyNote(c, p, "shield", tr("镜像不在 Fabo 里", "The image isn't in Fabo"),
			tr("它被推送或导出到了别处，或者之后删掉了。加 --load 构建后就能在这里扫描。", "It was pushed or exported elsewhere, or removed since. Build it with --load to scan it here."))
		return
	}
	b := &s.bl
	if _, known := s.scans[img.ID]; !known {
		if b.scanAsked == nil {
			b.scanAsked = map[string]bool{}
		}
		if !b.scanAsked[img.ID] {
			b.scanAsked[img.ID] = true
			s.run(15*time.Second, func(ctx context.Context) func() {
				scan, err := s.engine.lastScan(ctx, img.ref())
				return func() {
					// An unreadable last scan counts as none, so the tab
					// offers a scan instead of waiting for good.
					if err != nil {
						scan = nil
					}
					s.scans[img.ID] = scan
				}
			})
		}
		if _, scanning := s.scanning[img.ID]; !scanning {
			note(c, p, tr("正在读取扫描结果…", "Reading the last scan…"))
			return
		}
	}
	ui.Column(c).Fill().Grow(1).MinHeight(0).Children(func() {
		ui.Row(c.Key("scan-head")).FillWidth().Padding(10, 16, 0).Gap(8).AlignItems(ui.Center).Children(func() {
			ui.Text(c, img.ref()).Font(mono).FontSize(11.5).TextColor(p.muted).SingleLine().MinWidth(0).Grow(1)
			_, scanning := s.scanning[img.ID]
			if result := s.scans[img.ID]; result != nil {
				if button(c.Key("rescan"), p, tr("重新扫描", "Rescan"), "", ghostButton, scanning) {
					s.startScan(img)
				}
				s.exportMenu(c, p, img, result)
			}
		})
		s.scanPane(c, p, img)
	})
}

// buildInfoTab lists the build's record.
func (s *desktop) buildInfoTab(c *ui.Context, p palette, rec buildRecord, d *buildDetail) {
	src := rec
	var context, dockerfile string
	if d != nil {
		src, context, dockerfile = d.buildRecord, d.Context, d.Dockerfile
	}
	when := func(t time.Time) string {
		if t.IsZero() {
			return ""
		}
		return t.Local().Format("2006-01-02 15:04:05")
	}
	steps := fmt.Sprintf("%d/%d", src.Done, src.Total)
	if src.Cached > 0 {
		steps += fmt.Sprintf(tr("（%d 缓存）", " (%d cached)"), src.Cached)
	}
	keyValues(c, p, compact([]field{
		{tr("构建 ID", "Build ID"), rec.ID}, {tr("构建器", "Builder"), rec.Builder}, {tr("状态", "State"), buildWords(buildKind(src.Status))},
		{tr("上下文", "Context"), context}, {"Dockerfile", dockerfile},
		{tr("开始", "Started"), when(src.Started)}, {tr("完成", "Finished"), when(src.Completed)},
		{tr("耗时", "Duration"), buildDuration(buildSpan(src.Started, src.Completed))}, {tr("步骤", "Steps"), steps},
	}))
}
