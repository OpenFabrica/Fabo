package main

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/egoist/mygo/ui"
)

// importSource is an engine Fabo can import from, as `fabo import --sources`
// reports it: its docker context, its name, and whether it answers now.
type importSource struct {
	ID      string `json:"id"`
	Label   string `json:"label"`
	Running bool   `json:"running"`
}

// importItem is one image, volume or container of a source, as
// `fabo import <source> --json` lists it. Detail is an image's size, or a
// container's image and state.
type importItem struct {
	Name      string `json:"name"`
	Detail    string `json:"detail"`
	Have      bool   `json:"have"`
	Anonymous bool   `json:"anonymous"`
}

// importInventory is what a source holds, by kind.
type importInventory struct {
	Source     string       `json:"source"`
	Images     []importItem `json:"images"`
	Volumes    []importItem `json:"volumes"`
	Containers []importItem `json:"containers"`
}

// items are the inventory's entries of one kind ("image", "volume" or
// "container", the words `fabo import` takes).
func (v *importInventory) items(kind string) []importItem {
	switch kind {
	case "image":
		return v.Images
	case "volume":
		return v.Volumes
	}
	return v.Containers
}

// free are the entries of a kind worth bringing over: what Fabo lacks,
// leaving out anonymous volumes.
func (v *importInventory) free(kind string) []importItem {
	var out []importItem
	for _, item := range v.items(kind) {
		if !item.Have && !item.Anonymous {
			out = append(out, item)
		}
	}
	return out
}

// importStep is how one picked item's import went.
type importStep struct {
	state string // "running", "done" or "error"
	err   string
}

// dockerContext is which engine plain docker uses, as `fabo context`
// reports it: the current context, this installation's own, and its name.
type dockerContext struct {
	Current string `json:"current"`
	Ours    string `json:"ours"`
	App     string `json:"app"`
}

// switchCard follows docker's switch to Fabo: where it came from and the
// images on their way over from there.
type switchCard struct {
	from                string
	total, done, failed int
	finished            bool
}

// importUI is everything about bringing things over from OrbStack or Docker
// Desktop: Settings → Import, the strip under the lists, the review window,
// and the offer to switch plain docker to Fabo.
type importUI struct {
	sourcesRead, sourcesLoading bool
	sources                     []importSource
	source                      string
	inv                         *importInventory
	invLoading                  bool
	problem                     string
	// picked and steps are keyed "kind/name".
	picked  map[string]bool
	steps   map[string]importStep
	queue   []string
	done    int
	failed  int
	summary string

	// review is the review window; col the kind it lists. pending names the
	// engine docker switches from once the window's Import starts.
	review  bool
	col     string
	pending string

	ctx     dockerContext
	ctxRead bool
	// ask is the switch dialog, offered while docker uses another engine;
	// bring is its "bring all my images" tick, set once the images are known.
	ask, bringSet, bring bool
	card                 *switchCard
}

// importKinds are the three kinds in the order they import: images first,
// so the containers that use them find them there.
var importKinds = []struct{ kind, zh, en string }{
	{"image", "镜像", "Images"},
	{"volume", "卷", "Volumes"},
	{"container", "容器", "Containers"},
}

// ctxSnooze is how long "Not now" quiets the switch strip.
const ctxSnooze = 7 * 24 * time.Hour

// importInit makes the maps ready before anything reads or writes them.
func (s *desktop) importInit() {
	if s.imp.picked == nil {
		s.imp.picked, s.imp.steps = map[string]bool{}, map[string]importStep{}
	}
}

// importWatch reads, once the engine answers, which engine docker uses and
// what the other engines have, so the strip and the switch offer show
// without opening Settings.
func (s *desktop) importWatch() {
	if s.launcher == nil || !s.connected {
		return
	}
	s.importInit()
	if !s.imp.ctxRead {
		s.loadDockerContext()
	}
	if !s.imp.sourcesRead {
		s.loadImportSources()
	}
}

// loadDockerContext asks the launcher which context plain docker uses. A
// launcher that cannot say (Linux) leaves it empty, and nothing offers to
// switch.
func (s *desktop) loadDockerContext() {
	s.imp.ctxRead = true
	launcher := s.launcher
	s.run(10*time.Second, func(ctx context.Context) func() {
		var state dockerContext
		out, err := launcher(ctx, "context")
		if err == nil && json.Unmarshal(out, &state) == nil {
			return func() { s.imp.ctx = state }
		}
		return nil
	})
}

// loadImportSources asks the launcher which source engines this machine
// has, chooses one (the one docker uses, else a running one), and reads
// its inventory when it runs.
func (s *desktop) loadImportSources() {
	s.imp.sourcesRead, s.imp.sourcesLoading = true, true
	launcher := s.launcher
	s.run(30*time.Second, func(ctx context.Context) func() {
		var sources []importSource
		out, err := launcher(ctx, "import", "--sources")
		if err == nil {
			err = json.Unmarshal(out, &sources)
		}
		return func() {
			s.imp.sourcesLoading = false
			if err != nil {
				s.imp.problem = err.Error()
				return
			}
			s.imp.sources = sources
			if s.importSource() == nil {
				s.imp.source = ""
				for _, prefer := range []func(importSource) bool{
					func(x importSource) bool { return x.Running && x.ID == s.imp.ctx.Current },
					func(x importSource) bool { return x.Running },
					func(importSource) bool { return true },
				} {
					for _, x := range sources {
						if s.imp.source == "" && prefer(x) {
							s.imp.source = x.ID
						}
					}
				}
			}
			if src := s.importSource(); src != nil && src.Running {
				s.loadImportInventory()
			}
		}
	})
}

// importSource is the chosen source, or nil before one is found.
func (s *desktop) importSource() *importSource {
	return s.sourceByID(s.imp.source)
}

// sourceByID is the source with a context's name, or nil.
func (s *desktop) sourceByID(id string) *importSource {
	for i := range s.imp.sources {
		if s.imp.sources[i].ID == id {
			return &s.imp.sources[i]
		}
	}
	return nil
}

// chooseSource makes another source the chosen one: its picks start empty
// and its inventory is read when it runs.
func (s *desktop) chooseSource(id string) {
	s.imp.source, s.imp.inv, s.imp.problem, s.imp.summary = id, nil, "", ""
	s.imp.picked, s.imp.steps = map[string]bool{}, map[string]importStep{}
	if src := s.importSource(); src != nil && src.Running {
		s.loadImportInventory()
	}
}

// loadImportInventory reads what the chosen source has. Ticks of things
// that are now in Fabo are dropped.
func (s *desktop) loadImportInventory() {
	source, launcher := s.imp.source, s.launcher
	s.imp.invLoading, s.imp.problem = true, ""
	s.run(2*time.Minute, func(ctx context.Context) func() {
		var inv importInventory
		out, err := launcher(ctx, "import", source, "--json")
		if err == nil {
			err = json.Unmarshal(out, &inv)
		}
		return func() {
			s.imp.invLoading = false
			if source != s.imp.source {
				return
			}
			if err != nil {
				s.imp.inv, s.imp.problem = nil, err.Error()
				return
			}
			inv.Source = source
			s.imp.inv = &inv
			s.importInit()
			for key := range s.imp.picked {
				if !s.importFree(key) {
					delete(s.imp.picked, key)
				}
			}
		}
	})
}

// importFree reports whether "kind/name" is in the inventory and not in Fabo.
func (s *desktop) importFree(key string) bool {
	if s.imp.inv == nil {
		return false
	}
	kind, name := splitImportKey(key)
	for _, item := range s.imp.inv.items(kind) {
		if item.Name == name {
			return !item.Have
		}
	}
	return false
}

// importable counts, by kind, what the chosen source has that Fabo lacks,
// and their sum.
func (s *desktop) importable() (counts [3]int, total int) {
	if s.imp.inv == nil || s.imp.inv.Source != s.imp.source {
		return counts, 0
	}
	for i, k := range importKinds {
		counts[i] = len(s.imp.inv.free(k.kind))
		total += counts[i]
	}
	return counts, total
}

// importPicked lists the ticked items, in import order.
func (s *desktop) importPicked() []string {
	if s.imp.inv == nil {
		return nil
	}
	var keys []string
	for _, k := range importKinds {
		for _, item := range s.imp.inv.items(k.kind) {
			if key := k.kind + "/" + item.Name; s.imp.picked[key] && !item.Have {
				keys = append(keys, key)
			}
		}
	}
	return keys
}

// allPicked reports whether a kind has free items and all are ticked.
func (s *desktop) allPicked(kind string) bool {
	free := s.imp.inv.free(kind)
	for _, item := range free {
		if !s.imp.picked[kind+"/"+item.Name] {
			return false
		}
	}
	return len(free) > 0
}

// pickAll ticks every free item of a kind, or unticks them when all are
// ticked already.
func (s *desktop) pickAll(kind string) {
	all := s.allPicked(kind)
	for _, item := range s.imp.inv.free(kind) {
		if all {
			delete(s.imp.picked, kind+"/"+item.Name)
		} else {
			s.imp.picked[kind+"/"+item.Name] = true
		}
	}
}

// startImport imports the given items one by one. When the review window
// was opened from the switch dialog, docker switches to Fabo as it starts.
func (s *desktop) startImport(keys []string) {
	if len(keys) == 0 || len(s.imp.queue) > 0 || s.launcher == nil {
		return
	}
	s.imp.queue, s.imp.done, s.imp.failed, s.imp.summary = keys, 0, 0, ""
	if from := s.imp.pending; from != "" {
		s.imp.pending = ""
		s.switchDocker(from, len(keys))
	}
	s.importNext()
}

// importNext runs the head of the queue through `fabo import <source>
// <kind> <name>`, records how it went, and moves on. One item at a time
// keeps the progress honest and lets a failure stop only its own item.
// After the last item the inventory is read again, so what arrived shows
// as already in Fabo.
func (s *desktop) importNext() {
	if len(s.imp.queue) == 0 {
		s.imp.summary = fmt.Sprintf(tr("已导入 %d 项。", "Imported %d items."), s.imp.done)
		if s.imp.failed > 0 {
			s.imp.summary = fmt.Sprintf(tr("已导入 %d 项；%d 项失败。", "Imported %d; %d failed."), s.imp.done, s.imp.failed)
		}
		if s.imp.card != nil && s.imp.card.total > 0 {
			s.imp.card.finished = true
		}
		s.settle(nil, s.imp.summary)
		s.loadImportInventory()
		return
	}
	key, source, launcher := s.imp.queue[0], s.imp.source, s.launcher
	kind, name := splitImportKey(key)
	s.imp.steps[key] = importStep{state: "running"}
	// The CLI bounds one item at 30 minutes; give it a little longer.
	s.run(31*time.Minute, func(ctx context.Context) func() {
		_, err := launcher(ctx, "import", source, kind, name)
		return func() {
			card := s.imp.card
			counted := card != nil && card.total > 0
			if err != nil {
				s.imp.steps[key] = importStep{state: "error", err: err.Error()}
				s.imp.failed++
				if counted {
					card.failed++
				}
			} else {
				s.imp.steps[key] = importStep{state: "done"}
				s.imp.done++
				if counted {
					card.done++
				}
			}
			delete(s.imp.picked, key)
			s.imp.queue = s.imp.queue[1:]
			s.importNext()
		}
	})
}

// splitImportKey splits "kind/name"; names may themselves hold slashes.
func splitImportKey(key string) (kind, name string) {
	kind, name, _ = strings.Cut(key, "/")
	return kind, name
}

// contextLabel is how a docker context is called: the engines by name.
func contextLabel(id string) string {
	switch id {
	case "orbstack":
		return "OrbStack"
	case "desktop-linux":
		return "Docker Desktop"
	case "default":
		return tr("Docker 的默认引擎", "Docker's default engine")
	}
	return id
}

// dockerElsewhere reports whether plain docker uses an engine other than
// this Fabo.
func (s *desktop) dockerElsewhere() bool {
	c := s.imp.ctx
	return c.Current != "" && c.Ours != "" && c.Current != c.Ours
}

// appName is this installation's name in the switch texts.
func (s *desktop) appName() string {
	if s.imp.ctx.App != "" {
		return s.imp.ctx.App
	}
	return "Fabo"
}

// switchDocker makes plain docker use Fabo, remembering where it came from
// for Switch back; total items are on their way over from there.
func (s *desktop) switchDocker(from string, total int) {
	launcher, ours := s.launcher, s.imp.ctx.Ours
	s.imp.card = &switchCard{from: from, total: total}
	s.prefs.CtxBack = from
	s.savePrefs()
	s.run(15*time.Second, func(ctx context.Context) func() {
		_, err := launcher(ctx, "context", "use", ours)
		return func() {
			if err != nil {
				s.imp.card = nil
				s.settle(err, "")
				return
			}
			s.imp.ctx.Current = ours
		}
	})
}

// switchBack puts plain docker back on the engine it used before, and
// keeps the switch strip quiet for a week.
func (s *desktop) switchBack() {
	launcher, back := s.launcher, s.prefs.CtxBack
	if back == "" {
		back = "default"
	}
	s.imp.card = nil
	s.prefs.CtxSnooze = time.Now().Add(ctxSnooze).Unix()
	s.savePrefs()
	s.run(15*time.Second, func(ctx context.Context) func() {
		_, err := launcher(ctx, "context", "use", back)
		return func() {
			if err != nil {
				s.settle(err, "")
				return
			}
			s.imp.ctx.Current = back
		}
	})
}

// sizePattern reads the docker CLI's image sizes: "1.2GB", "187MB", "5.6kB".
var sizePattern = regexp.MustCompile(`^([\d.]+)\s*([kKMGT]?)B$`)

// dockerBytes is a docker CLI size in bytes, or 0 when it isn't one.
func dockerBytes(text string) int64 {
	m := sizePattern.FindStringSubmatch(strings.TrimSpace(text))
	if m == nil {
		return 0
	}
	n, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return 0
	}
	if m[2] != "" {
		n *= map[string]float64{"K": 1e3, "M": 1e6, "G": 1e9, "T": 1e12}[strings.ToUpper(m[2])]
	}
	return int64(n)
}

// countWords says how many images, volumes and containers, as "3 images,
// 1 volumes and 2 containers", leaving out kinds with none.
func countWords(counts [3]int) string {
	var parts []string
	for i, format := range []string{tr("%d 个镜像", "%d images"), tr("%d 个卷", "%d volumes"), tr("%d 个容器", "%d containers")} {
		if counts[i] > 0 {
			parts = append(parts, fmt.Sprintf(format, counts[i]))
		}
	}
	if len(parts) < 2 {
		return strings.Join(parts, "")
	}
	return strings.Join(parts[:len(parts)-1], tr("、", ", ")) + tr("和", " and ") + parts[len(parts)-1]
}

// importPane is Settings → Import: images, volumes and containers from
// OrbStack or Docker Desktop. The source is only read; containers arrive
// stopped.
func (s *desktop) importPane(c *ui.Context, p palette) {
	heading(c, p, tr("导入", "Import"))
	if s.launcher == nil {
		remark(c, p, tr("导入需要本安装的 fabo 命令。", "Importing needs this installation's fabo command."))
		return
	}
	s.importInit()
	if !s.imp.sourcesRead {
		s.loadImportSources()
	}
	busy := len(s.imp.queue) > 0
	switch {
	case s.imp.sourcesLoading:
		remark(c, p, tr("正在查找 OrbStack 和 Docker Desktop…", "Looking for OrbStack and Docker Desktop…"))
		return
	case len(s.imp.sources) == 0:
		if s.imp.problem != "" {
			remark(c, p, s.imp.problem)
		} else {
			remark(c, p, tr("这台电脑上没有 OrbStack 或 Docker Desktop，没有可导入的内容。", "Nothing to import from: OrbStack and Docker Desktop aren't on this computer."))
		}
		return
	}
	src := s.importSource()
	pref(c, p, tr("来源", "From"), fmt.Sprintf(tr("把镜像、卷和容器带到 %s。", "Bring images, volumes and containers over to %s."), s.appName()), func() {
		labels := make([]string, len(s.imp.sources))
		idx := 0
		for i, x := range s.imp.sources {
			labels[i] = x.Label
			if x.ID == s.imp.source {
				idx = i
			}
		}
		if choice(c.Key("import-from"), p, tr("导入来源", "Import from"), &idx, labels...) && !busy {
			s.chooseSource(s.imp.sources[idx].ID)
		}
		if button(c.Key("import-reload"), p, tr("刷新", "Refresh"), "", plainButton, busy || s.imp.invLoading) {
			s.imp.inv, s.imp.problem = nil, ""
			s.loadImportSources()
		}
	})
	switch {
	case src != nil && !src.Running:
		remark(c, p, fmt.Sprintf(tr("%s 未运行。启动它后再导入，导入完成后它不必一直开着。", "%s isn't running. Start it to import from it; it doesn't need to stay open afterwards."), src.Label))
		return
	case !s.connected:
		remark(c, p, tr("启动 Fabo 后才能导入。", "Start Fabo to import into it."))
		return
	case s.imp.problem != "":
		remark(c, p, s.imp.problem)
		return
	case s.imp.inv == nil:
		if !s.imp.invLoading {
			s.loadImportInventory()
		}
		remark(c, p, fmt.Sprintf(tr("正在读取 %s 的内容…", "Reading what %s has…"), src.Label))
		return
	}
	for _, k := range importKinds {
		ui.Column(c.Key("imp-"+k.kind)).FillWidth().Gap(6).Padding(0, 0, 14).BorderWidth(0, 0, 1, 0).BorderColor(p.line).Children(func() {
			s.importHead(c, p, k.kind, tr(k.zh, k.en), busy)
			s.importRows(c, p, k.kind, busy)
		})
	}
	s.importFoot(c, p, src.Label, busy)
}

// importHead is a kind's title over its list: the count, and Select all
// or Select none when there is something to tick.
func (s *desktop) importHead(c *ui.Context, p palette, kind, title string, busy bool) {
	ui.Row(c.Key("head-" + kind)).FillWidth().Height(26).Gap(8).AlignItems(ui.Center).Children(func() {
		ui.Text(c, title).FontSize(13).FontWeight(600).TextColor(p.text)
		label(c, strconv.Itoa(len(s.imp.inv.items(kind))), p.muted)
		ui.Box(c).Grow(1)
		if len(s.imp.inv.free(kind)) > 0 {
			text := tr("全选", "Select all")
			if s.allPicked(kind) {
				text = tr("全不选", "Select none")
			}
			b := clickable(c.Key("all")).Label(text + " · " + title).Disabled(busy).PaddingX(4)
			ink := p.link
			if busy {
				ink = p.muted
			}
			b.Children(func() { ui.Text(c, text).FontSize(12).TextColor(ink) })
			if b.Clicked() {
				s.pickAll(kind)
			}
		}
	})
}

// importRows lists one kind's items, or says there are none.
func (s *desktop) importRows(c *ui.Context, p palette, kind string, busy bool) {
	items := s.imp.inv.items(kind)
	if len(items) == 0 {
		label(c, tr("没有", "Nothing here"), p.muted)
	}
	for _, item := range items {
		s.importRow(c.Key(kind+"/"+item.Name), p, kind, item, busy)
	}
}

// importRow is one item: its tick and name, what it is, and how its
// import went.
func (s *desktop) importRow(c *ui.Context, p palette, kind string, item importItem, busy bool) {
	key := kind + "/" + item.Name
	step := s.imp.steps[key]
	ui.Row(c).FillWidth().Gap(10).AlignItems(ui.Center).Children(func() {
		on := s.imp.picked[key] && !item.Have
		ui.Checkbox(c.Key("tick"), &on, item.Name).Disabled(busy || item.Have)
		if !item.Have && !busy {
			if on {
				s.imp.picked[key] = true
			} else {
				delete(s.imp.picked, key)
			}
		}
		ui.Box(c).Grow(1)
		switch {
		case item.Have:
			label(c, fmt.Sprintf(tr("已在 %s 中", "In %s"), s.appName()), p.muted)
		case item.Anonymous:
			label(c, tr("匿名", "anonymous"), p.muted)
		case item.Detail != "":
			label(c, item.Detail, p.muted)
		}
		switch step.state {
		case "running":
			label(c, tr("导入中…", "Importing…"), p.accent)
		case "done":
			label(c, tr("已导入", "Imported"), p.ok)
		case "error":
			label(c, tr("导入失败", "Import failed"), p.err)
		}
	})
	if step.state == "error" {
		ui.Text(c.Key(key+"-err"), step.err).FontSize(11.5).LineHeight(1.4).TextColor(p.err).Selectable()
	}
}

// importGo is the Import button: how many items it brings, and whether it
// also switches docker.
func (s *desktop) importGo(c *ui.Context, p palette, busy bool) {
	count := len(s.importPicked())
	title := tr("导入", "Import")
	switch {
	case busy:
		title = tr("导入中…", "Importing…")
	case count > 0 && s.imp.pending != "":
		title = fmt.Sprintf(tr("切换并导入 %d 项", "Switch and import %d items"), count)
	case count > 0:
		title = fmt.Sprintf(tr("导入 %d 项", "Import %d items"), count)
	case s.imp.pending != "":
		title = tr("切换并导入", "Switch and import")
	}
	if button(c.Key("import-start"), p, title, "import", primaryButton, busy || count == 0) {
		s.startImport(s.importPicked())
	}
}

// importFoot is the line under the lists: the last import's outcome, or
// what importing leaves alone, and the Import button.
func (s *desktop) importFoot(c *ui.Context, p palette, name string, busy bool) {
	ui.Row(c.Key("import-go")).FillWidth().Gap(16).AlignItems(ui.Center).Children(func() {
		text := s.imp.summary
		switch {
		case busy:
			text = fmt.Sprintf(tr("正在导入，剩余 %d 项…", "Importing, %d left…"), len(s.imp.queue))
		case text == "":
			text = fmt.Sprintf(tr("%s 不会被改动：镜像是复制过来的，卷通过一个只读的临时辅助容器读取，用完即删。容器到达时处于停止状态。",
				"%s isn't changed: images are copied, and volumes are read through a temporary read-only helper that's removed after. Containers arrive stopped."), name)
		}
		ui.Text(c, text).FontSize(12).LineHeight(1.45).TextColor(p.muted).Grow(1).Width(0)
		s.importGo(c, p, busy)
	})
}

// stripButton is the small button of the strip under the lists.
func stripButton(c *ui.Context, p palette, title string) bool {
	b := clickable(c).Label(title).Height(22).PaddingX(9).Radius(6).Shrink(0).AlignItems(ui.Center)
	bg := p.accent
	if b.Hovered() {
		bg = bg.Mix(p.text, 0.1)
	}
	b.Background(bg).Children(func() { ui.Text(c, title).FontSize(11.5).FontWeight(500).TextColor(p.onAccent) })
	return b.Clicked()
}

// importStrip is the thin bar under the lists. While docker uses another
// engine it offers to switch (Containers and Images only); otherwise it
// says what the source has that Fabo lacks. Closing either keeps it closed:
// the switch for a week, the import until the source has more.
func (s *desktop) importStrip(c *ui.Context, p palette) {
	// While a window or the card says it, the strip would say it twice.
	if s.page == settingsPage || s.page == terminalPage || s.imp.card != nil || s.imp.ask || s.imp.review || s.launcher == nil || !s.connected {
		return
	}
	bar := func(key, icon string, ink, tint ui.Color, bold, rest string, build func()) {
		ui.Row(c.Key(key)).FillWidth().Height(30).Shrink(0).Padding(0, 8, 0, 18).Gap(10).AlignItems(ui.Center).
			BorderWidth(1, 0, 0, 0).BorderColor(p.line).Background(tint).Children(func() {
			ui.Icon(c, icons[icon]).Size(14, 14).TextColor(ink).Shrink(0)
			ui.Row(c).Grow(1).Width(0).MinWidth(0).Gap(4).AlignItems(ui.Center).Clip().Children(func() {
				ui.Text(c, bold).FontSize(12).FontWeight(600).TextColor(p.text).SingleLine().Shrink(0)
				ui.Text(c, rest).FontSize(12).TextColor(p.muted).SingleLine().MinWidth(0)
			})
			build()
		})
	}
	if s.dockerElsewhere() && (s.page == containersPage || s.page == imagesPage) && time.Now().Unix() >= s.prefs.CtxSnooze {
		engine := contextLabel(s.imp.ctx.Current)
		bar("ctx-strip", "info", p.warn, p.warn.Alpha(0.08),
			fmt.Sprintf(tr("docker 仍在使用 %s，", "docker still uses %s,"), engine),
			tr("你在终端里运行的内容不会出现在这里。你的镜像可以一起带过来。", "so what you run in the terminal won't show up here. Your images can come along."), func() {
				if stripButton(c.Key("ctx-switch"), p, fmt.Sprintf(tr("切换到 %s", "Switch to %s"), s.appName())) {
					s.openSwitch()
				}
				if iconButton(c.Key("ctx-snooze"), p, "x", tr("暂不：一周后再问", "Not now: ask again in a week"), false).Clicked() {
					s.prefs.CtxSnooze = time.Now().Add(ctxSnooze).Unix()
					s.savePrefs()
				}
			})
		return
	}
	counts, total := s.importable()
	src := s.importSource()
	seen, ok := s.prefs.ImportSeen[s.imp.source]
	if src == nil || total == 0 || (ok && total <= seen) {
		return
	}
	bar("import-strip", "import", p.accent, ui.Transparent,
		fmt.Sprintf(tr("%s 有 %s", "%s has %s"), src.Label, countWords(counts)),
		fmt.Sprintf(tr("还不在 %s 中。", "that aren't in %s yet."), s.appName()), func() {
			if stripButton(c.Key("import-review"), p, tr("查看", "Review")) {
				s.openReview("")
			}
			if iconButton(c.Key("import-dismiss"), p, "x", fmt.Sprintf(tr("隐藏，直到 %s 有更多内容", "Hide until %s has more"), src.Label), false).Clicked() {
				if s.prefs.ImportSeen == nil {
					s.prefs.ImportSeen = map[string]int{}
				}
				s.prefs.ImportSeen[s.imp.source] = total
				s.savePrefs()
			}
		})
}

// openReview opens the review window on the chosen source. With nothing
// ticked yet it ticks the images Fabo lacks, the usual thing to bring.
// pending, when set, is the engine docker switches from as it imports.
func (s *desktop) openReview(pending string) {
	s.importInit()
	if len(s.importPicked()) == 0 && s.imp.inv != nil {
		for _, item := range s.imp.inv.free("image") {
			s.imp.picked["image/"+item.Name] = true
		}
	}
	s.imp.review, s.imp.pending, s.imp.col = true, pending, ""
}

// reviewWindow is the review window: the kinds on the left, ticked when all
// of a kind is, and the chosen kind's items on the right.
func (s *desktop) reviewWindow(c *ui.Context, p palette) {
	open := s.imp.review && s.imp.inv != nil
	if !open {
		s.imp.review, s.imp.pending = false, ""
		return
	}
	busy := len(s.imp.queue) > 0
	ui.DialogBase(c.Key("import-review"), &open, func(backdrop, dialog ui.Element) {
		backdrop.Background(ui.RGBA(0, 0, 0, 0.45))
		dialog.Width(640).Padding(18).Gap(14).Radius(10).Background(p.panel).Border(1, p.line).
			Shadow(0, 24, 60, -10, ui.RGBA(0, 0, 0, 0.5))
		name := contextLabel(s.imp.source)
		if src := s.importSource(); src != nil {
			name = src.Label
		}
		counts, total := s.importable()
		if s.imp.col == "" {
			s.imp.col = "image"
			for i, k := range importKinds {
				if counts[i] > 0 {
					s.imp.col = k.kind
					break
				}
			}
		}
		ui.Row(c.Key("review-head")).FillWidth().Gap(12).AlignItems(ui.Center).Children(func() {
			ui.Box(c).Size(40, 40).Radius(10).Background(p.accent.Alpha(0.12)).Center().Shrink(0).Children(func() {
				ui.Icon(c, icons["import"]).Size(20, 20).TextColor(p.accent)
			})
			ui.Column(c).Grow(1).Width(0).Gap(3).Children(func() {
				ui.Text(c, fmt.Sprintf(tr("把 %s 的内容带到 %s", "Bring your %s things to %s"), name, s.appName())).FontSize(14).FontWeight(600).TextColor(p.text)
				sub := fmt.Sprintf(tr("%d 项还不在 %s 中，选择要复制的内容。", "%d items aren't in %s yet. Pick what to copy over."), total, s.appName())
				if total == 0 {
					sub = fmt.Sprintf(tr("全部都已在 %s 中。", "Everything is in %s."), s.appName())
				}
				ui.Text(c, sub).FontSize(12).TextColor(p.muted)
			})
		})
		ui.Row(c.Key("review-cols")).FillWidth().Height(300).AlignItems(ui.Stretch).Radius(8).Border(1, p.line).Clip().Children(func() {
			ui.Column(c.Key("kinds")).Width(200).Shrink(0).Padding(6).Gap(2).Background(p.bg).BorderWidth(0, 1, 0, 0).BorderColor(p.line).Children(func() {
				for i, k := range importKinds {
					s.reviewKind(c.Key(k.kind), p, k.kind, tr(k.zh, k.en), counts[i], busy)
				}
			})
			ui.Column(c.Key("items")).Grow(1).Width(0).MinWidth(0).Padding(10, 12).Gap(8).Children(func() {
				title := ""
				for _, k := range importKinds {
					if k.kind == s.imp.col {
						title = tr(k.zh, k.en)
					}
				}
				s.importHead(c, p, s.imp.col, title, busy)
				ui.Scroll(c.Key("list-" + s.imp.col)).FillWidth().Grow(1).MinHeight(0).Children(func() {
					ui.Column(c).FillWidth().Gap(6).Children(func() { s.importRows(c, p, s.imp.col, busy) })
				})
			})
		})
		note := s.imp.summary
		if busy {
			note = fmt.Sprintf(tr("正在导入，剩余 %d 项…", "Importing, %d left…"), len(s.imp.queue))
		} else if note == "" {
			note = fmt.Sprintf(tr("%s 保持原样：%s 只复制。容器到达时处于停止状态。", "%s stays as it is: %s only copies. Containers arrive stopped."), name, s.appName())
		}
		ui.Text(c, note).FontSize(12).LineHeight(1.45).TextColor(p.muted)
		ui.Row(c).FillWidth().Gap(8).Justify(ui.End).Children(func() {
			closeText := tr("关闭", "Close")
			if s.imp.summary != "" && !busy {
				closeText = tr("完成", "Done")
			}
			if button(c.Key("review-close"), p, closeText, "", plainButton, false) {
				open = false
			}
			s.importGo(c, p, busy)
		})
	})
	if !open {
		s.imp.review, s.imp.pending = false, ""
	}
}

// reviewKind is one kind in the review window's left column: its tick
// (all of it, or none), its name, and how many of it are ticked.
func (s *desktop) reviewKind(c *ui.Context, p palette, kind, title string, free int, busy bool) {
	picked := 0
	for key := range s.imp.picked {
		if k, _ := splitImportKey(key); k == kind && s.importFree(key) {
			picked++
		}
	}
	b := clickable(c).Label(title).FillWidth().Height(34).PaddingX(8).Gap(8).Radius(6).AlignItems(ui.Center)
	if s.imp.col == kind {
		b.Background(p.soft)
	} else if b.Hovered() {
		b.Background(p.hover)
	}
	b.Children(func() {
		all := s.allPicked(kind)
		was := all
		ui.Checkbox(c.Key("all"), &all, "").Label(fmt.Sprintf(tr("全部%s", "All %s"), strings.ToLower(title))).Disabled(free == 0 || busy)
		if all != was {
			s.pickAll(kind)
			s.imp.col = kind
		}
		ui.Text(c, title).FontSize(12.5).FontWeight(500).TextColor(p.text).Grow(1)
		count := strconv.Itoa(len(s.imp.inv.items(kind)))
		if picked > 0 {
			count = fmt.Sprintf(tr("%d / %d", "%d of %d"), picked, free)
		}
		label(c, count, p.muted)
		ui.Icon(c, icons["chevron"]).Size(12, 12).TextColor(p.muted)
	})
	if b.Clicked() {
		s.imp.col = kind
	}
}

// openSwitch opens the switch dialog for the engine docker uses now, and
// reads that engine's inventory when it is a running source.
func (s *desktop) openSwitch() {
	from := s.imp.ctx.Current
	s.importInit()
	s.imp.ask, s.imp.bringSet = true, false
	if src := s.sourceByID(from); src != nil && src.Running && (s.imp.inv == nil || s.imp.source != from) {
		s.chooseSource(from)
	}
}

// switchImages are the images the engine docker uses has and Fabo lacks,
// once its inventory is in; nil before.
func (s *desktop) switchImages(from string) []importItem {
	if s.imp.inv == nil || s.imp.source != from || s.imp.inv.Source != from {
		return nil
	}
	out := []importItem{}
	for _, item := range s.imp.inv.Images {
		if !item.Have {
			out = append(out, item)
		}
	}
	return out
}

// switchDialog asks to make plain docker use Fabo, says what that changes
// and keeps, and offers to bring the other engine's images along.
func (s *desktop) switchDialog(c *ui.Context, p palette) {
	open := s.imp.ask && s.dockerElsewhere()
	if !open {
		s.imp.ask = false
		return
	}
	from := s.imp.ctx.Current
	engine, app := contextLabel(from), s.appName()
	src, imgs := s.sourceByID(from), s.switchImages(from)
	var bytes int64
	for _, item := range imgs {
		bytes += dockerBytes(item.Detail)
	}
	// A lot to copy (over 10 GB) isn't ticked for you: choosing is likely better.
	big := bytes > 10e9
	if imgs != nil && !s.imp.bringSet {
		s.imp.bring, s.imp.bringSet = !big, true
	}
	ui.DialogBase(c.Key("switch-docker"), &open, func(backdrop, dialog ui.Element) {
		backdrop.Background(ui.RGBA(0, 0, 0, 0.45))
		dialog.Width(480).Padding(18).Gap(14).Radius(10).Background(p.panel).Border(1, p.line).
			Shadow(0, 24, 60, -10, ui.RGBA(0, 0, 0, 0.5))
		ui.Text(c, fmt.Sprintf(tr("让 docker 使用 %s？", "Use %s for docker?"), app)).FontSize(14).FontWeight(600).TextColor(p.text)
		theirs := ""
		if imgs != nil {
			theirs = fmt.Sprintf(tr("%d 个镜像 · %d 个容器", "%d images · %d containers"), len(s.imp.inv.Images), len(s.imp.inv.Containers))
		} else if src != nil && !src.Running {
			theirs = tr("未运行", "not running")
		}
		mine := fmt.Sprintf(tr("%d 个镜像 · %d 个容器", "%d images · %d containers"), len(s.images), len(s.containers))
		ui.Row(c.Key("flow")).FillWidth().Gap(10).AlignItems(ui.Center).Children(func() {
			end := func(key, name, sub string) {
				ui.Column(c.Key(key)).Grow(1).Width(0).Padding(10, 12).Gap(3).Radius(8).Background(p.bg).Border(1, p.line).Children(func() {
					ui.Text(c, name).FontSize(13).FontWeight(600).TextColor(p.text)
					if sub != "" {
						ui.Text(c, sub).FontSize(11.5).TextColor(p.muted)
					}
				})
			}
			end("from", engine, theirs)
			ui.Icon(c, icons["chevron"]).Size(16, 16).TextColor(p.muted)
			end("to", app, mine)
		})
		ui.Column(c.Key("keep")).FillWidth().Gap(8).Children(func() {
			line := func(key, icon string, ink ui.Color, text string) {
				ui.Row(c.Key(key)).FillWidth().Gap(8).AlignItems(ui.Start).Children(func() {
					ui.Icon(c, icons[icon]).Size(14, 14).TextColor(ink).Shrink(0)
					ui.Text(c, text).FontSize(12).LineHeight(1.45).TextColor(p.sub).Grow(1).Width(0)
				})
			}
			line("runs", "check", p.ok, fmt.Sprintf(tr("从现在起，docker、docker compose 和你的工具都在 %s 上运行。", "docker, docker compose and your tools run on %s from now on."), app))
			line("stays", "info", p.warn, fmt.Sprintf(tr("%[1]s 的镜像和容器留在 %[1]s 里；带过来之前 %[2]s 看不到它们。", "Your %[1]s images and containers stay in %[1]s. %[2]s can't see them until you bring them over."), engine, app))
			line("nothing", "check", p.ok, tr("不会删除任何东西，随时可以切回去。", "Nothing is deleted. You can switch back any time."))
		})
		switch {
		case len(imgs) > 0:
			ui.Column(c.Key("bring")).FillWidth().Gap(4).Padding(10, 12).Radius(8).Border(1, p.line).Children(func() {
				ui.Checkbox(c.Key("tick"), &s.imp.bring, fmt.Sprintf(tr("把我在 %s 的全部镜像带到 %s", "Bring all my %s images to %s"), engine, app))
				size := fmt.Sprintf(tr("%d 个镜像。", "%d images."), len(imgs))
				if bytes > 0 {
					size = fmt.Sprintf(tr("%d 个镜像，共 %s。", "%d images, %s."), len(imgs), displaySize(bytes))
				}
				hint := tr("复制期间可以继续工作。", "You can keep working while they copy.")
				if big {
					hint = tr("要复制的内容很多，可能需要很久；只选需要的会更快。", "That's a lot to copy and can take a long time; choosing what you need is quicker.")
				}
				ui.Text(c, size+" "+hint).FontSize(11.5).LineHeight(1.45).TextColor(p.muted).Padding(0, 0, 0, 24)
			})
		case src != nil && !src.Running:
			ui.Text(c, fmt.Sprintf(tr("启动 %s 即可现在带来它的镜像，也可以稍后在设置 → 导入里操作。", "Start %s to bring its images over now, or later from Settings → Import."), engine)).FontSize(12).TextColor(p.muted)
		}
		ui.Row(c.Key("foot")).FillWidth().Gap(8).AlignItems(ui.Center).Children(func() {
			if len(imgs) > 0 && button(c.Key("choose"), p, tr("选择要带的内容…", "Choose what to bring…"), "", ghostButton, false) {
				open = false
				s.openReview(from)
			}
			ui.Box(c).Grow(1)
			if button(c.Key("not-now"), p, tr("暂不", "Not now"), "", plainButton, false) {
				open = false
			}
			if button(c.Key("switch"), p, fmt.Sprintf(tr("切换到 %s", "Switch to %s"), app), "", primaryButton, false) {
				open = false
				s.switchWithImages(from, imgs)
			}
		})
	})
	if !open {
		s.imp.ask = false
	}
}

// switchWithImages switches docker to Fabo, and with "bring all" ticked
// starts bringing the other engine's images over.
func (s *desktop) switchWithImages(from string, imgs []importItem) {
	if !s.imp.bring || len(imgs) == 0 || len(s.imp.queue) > 0 {
		s.switchDocker(from, 0)
		return
	}
	keys := make([]string, len(imgs))
	for i, item := range imgs {
		keys[i] = "image/" + item.Name
		s.imp.picked[keys[i]] = true
	}
	s.switchDocker(from, len(keys))
	s.startImport(keys)
}

// switchCardView is the card in the corner after docker switched: the
// images on their way over, Switch back, and Show images.
func (s *desktop) switchCardView(c *ui.Context, p palette) {
	card := s.imp.card
	if card == nil {
		return
	}
	engine, app := contextLabel(card.from), s.appName()
	ui.Column(c.Key("switch-card")).Absolute().Right(16).Bottom(16).Width(340).Padding(14).Gap(10).Radius(10).
		Background(p.panel).Border(1, p.line).Shadow(0, 12, 32, -8, ui.RGBA(0, 0, 0, 0.35)).Children(func() {
		ui.Row(c).FillWidth().Gap(10).AlignItems(ui.Start).Children(func() {
			ui.Box(c).Size(26, 26).Radius(13).Background(p.ok.Alpha(0.14)).Center().Shrink(0).Children(func() {
				ui.Icon(c, icons["check"]).Size(14, 14).TextColor(p.ok)
			})
			ui.Column(c).Grow(1).Width(0).Gap(4).Children(func() {
				ui.Text(c, fmt.Sprintf(tr("docker 现在使用 %s", "docker now uses %s"), app)).FontSize(13).FontWeight(600).TextColor(p.text)
				n := card.done + card.failed
				var body string
				switch {
				case card.total == 0:
					body = fmt.Sprintf(tr("%s 的镜像仍留在那里，随时可以在设置 → 导入里带过来。", "Your %s images stay there. Bring them over any time from Settings → Import."), engine)
				case card.finished && card.failed > 0:
					body = fmt.Sprintf(tr("已从 %[2]s 带来 %[1]d 项，%[3]d 项失败（设置 → 导入里有原因）。", "Brought %[1]d items over from %[2]s; %[3]d failed (Settings → Import says why)."), card.done, engine, card.failed)
				case card.finished:
					body = fmt.Sprintf(tr("已从 %[2]s 带来 %[1]d 项。", "Brought %[1]d items over from %[2]s."), card.done, engine)
				default:
					body = fmt.Sprintf(tr("正在从 %s 带来你的镜像：%d / %d。", "Bringing your images over from %s: %d of %d."), engine, n, card.total)
				}
				ui.Text(c, body).FontSize(12).LineHeight(1.45).TextColor(p.muted)
				if card.total > 0 && !card.finished {
					meter(c, p, float64(n)/float64(card.total), 4, p.accent)
					ui.Text(c, tr("镜像到达后会出现在镜像列表里。", "They appear in Images as they arrive.")).FontSize(11.5).TextColor(p.muted)
				}
			})
			if iconButton(c.Key("card-close"), p, "x", tr("关闭", "Close"), false).Clicked() {
				s.imp.card = nil
			}
		})
		ui.Row(c).FillWidth().Gap(8).Justify(ui.End).Children(func() {
			if button(c.Key("switch-back"), p, tr("切回", "Switch back"), "", plainButton, false) {
				s.switchBack()
			}
			if card.total > 0 && button(c.Key("show-images"), p, tr("查看镜像", "Show images"), "", plainButton, false) {
				s.navigate(imagesPage)
			}
		})
	})
}
