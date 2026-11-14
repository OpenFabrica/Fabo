package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/egoist/mygo/ui"
)

// settingsTabs are the sections of Settings.
var settingsTabs = []struct{ id, icon, zh, en string }{
	{"appearance", "palette", "外观", "Appearance"},
	{"resources", "cpu", "资源", "Resources"},
	{"network", "globe", "网络", "Network"},
	{"builds", "hammer", "构建", "Builds"},
	{"domains", "link", "域名", "Domains"},
	{"registries", "layers", "镜像仓库", "Registries"},
	{"kubernetes", "helm", "Kubernetes", "Kubernetes"},
	{"about", "info", "关于", "About"},
}

// The steps of the two text sizes, as Fabo offers them.
var (
	textScales = []float32{0.9, 1, 1.1, 1.25}
	logSizes   = []float32{11, 13, 15}
)

// osName is how the host system is called in the settings' explanations.
func osName() string {
	switch runtime.GOOS {
	case "darwin":
		return "macOS"
	case "windows":
		return "Windows"
	}
	return "Linux"
}

// nearest is the index of the step closest to v.
func nearest(steps []float32, v float32) int {
	best := 0
	for i, step := range steps {
		if math.Abs(float64(step-v)) < math.Abs(float64(steps[best]-v)) {
			best = i
		}
	}
	return best
}

// loadSettings reads what the settings pages edit and resets their drafts.
// It runs when Settings opens, when the engine comes or goes, and after
// every change, so a saved value is shown as the engine stored it.
func (s *desktop) loadSettings() {
	s.settingsFor = strconv.FormatBool(s.connected)
	s.run(8*time.Second, func(ctx context.Context) func() {
		e := s.engine
		var res vmResources
		var proxy proxyInfo
		var domains domainInfo
		var builder builderInfo
		var version versionInfo
		resErr := e.get(ctx, "/fabo/resources", &res)
		proxyErr := e.get(ctx, "/fabo/proxy", &proxy)
		domainsErr := e.get(ctx, "/fabo/domains", &domains)
		builderErr := e.get(ctx, "/fabo/builder", &builder)
		versionErr := e.get(ctx, "/version", &version)
		var names nameState
		namesErr := errors.New("no launcher")
		if s.launcher != nil {
			var out []byte
			if out, namesErr = s.launcher(ctx, "domains"); namesErr == nil {
				namesErr = json.Unmarshal(out, &names)
			}
		}
		return func() {
			// An engine running directly on Linux answers with zero CPUs:
			// there is no VM to size.
			if s.resOK = resErr == nil && res.CPUs > 0; s.resOK {
				s.res, s.cpus, s.memory = res, float64(res.CPUs), float64(res.Memory)/1024
			}
			if s.proxyOK = proxyErr == nil && proxy.Settings.Mode != ""; s.proxyOK {
				s.proxy, s.proxyDraft = proxy, proxy.Settings
			}
			if s.domainsOK = domainsErr == nil && domains.HTTP.Port > 0; s.domainsOK {
				s.domains = domains
				s.portHTTP, s.portHTTPS = strconv.Itoa(domains.HTTP.Port), strconv.Itoa(domains.HTTPS.Port)
			}
			if s.builderOK = builderErr == nil; s.builderOK {
				s.builder, s.cacheGB = builder, strconv.FormatFloat(builder.CacheGB, 'f', -1, 64)
			}
			if versionErr == nil {
				s.version = version
			}
			if s.namesOK = namesErr == nil && names.Suffix != ""; s.namesOK {
				s.names = names
			}
		}
	})
}

// settingsView is the settings page: the sections on the left, the chosen
// one beside them, laid out as Fabo's settings are.
func (s *desktop) settingsView(c *ui.Context, p palette) {
	if s.settingsFor != strconv.FormatBool(s.connected) {
		s.loadSettings()
	}
	s.settingsTab = min(max(s.settingsTab, 0), len(settingsTabs)-1)
	tab := settingsTabs[s.settingsTab]
	ui.Scroll(c.Key("settings")).Fill().Grow(1).MinHeight(0).Children(func() {
		ui.Row(c).FillWidth().MaxWidth(940).Padding(4, 28, 32, 16).Gap(32).AlignItems(ui.Start).Children(func() {
			ui.Column(c.Key("tabs")).Width(168).Shrink(0).Gap(2).Children(func() {
				for i, t := range settingsTabs {
					if s.settingsTabButton(c.Key(t.id), p, t.icon, tr(t.zh, t.en), i == s.settingsTab) {
						s.settingsTab = i
					}
				}
			})
			ui.Column(c.Key("pane-" + tab.id)).Grow(1).Width(0).MinWidth(0).Gap(18).Children(func() {
				switch tab.id {
				case "appearance":
					s.appearancePane(c, p)
				case "resources":
					s.resourcesPane(c, p)
				case "network":
					s.networkPane(c, p)
				case "builds":
					heading(c, p, "BuildKit")
					s.builderRows(c, p)
				case "domains":
					s.domainsPane(c, p)
				case "registries":
					s.registryPane(c, p)
				case "kubernetes":
					s.kubernetesPane(c, p)
				case "about":
					s.aboutPane(c, p)
				}
			})
		})
	})
}

// settingsTabButton is one section of the settings' left column.
func (s *desktop) settingsTabButton(c *ui.Context, p palette, icon, label string, on bool) bool {
	b := clickable(c).Label(label).FillWidth().Height(34).PaddingX(10).Gap(10).Radius(8).AlignItems(ui.Center).Justify(ui.Start)
	ink, mark, weight := p.sub, p.muted, 400
	if on {
		b.Background(p.soft)
		ink, mark, weight = p.text, p.accent, 500
	} else if b.Hovered() {
		b.Background(p.hover)
		ink = p.text
	}
	b.Children(func() {
		ui.Icon(c, icons[icon]).Size(16, 16).TextColor(mark)
		ui.Text(c, label).FontSize(13).FontWeight(weight).TextColor(ink).SingleLine()
	})
	return b.Clicked()
}

// heading is the title of a settings pane.
func heading(c *ui.Context, p palette, title string) {
	ui.Text(c, title).FontSize(15).FontWeight(600).TextColor(p.text)
}

// pref is one setting as Fabo lays it out: a bold title over its muted
// explanation, the control on the right, and a rule beneath.
func pref(c *ui.Context, p palette, title, hint string, control func()) {
	ui.Row(c).FillWidth().Padding(0, 0, 16).Gap(24).AlignItems(ui.Center).
		BorderWidth(0, 0, 1, 0).BorderColor(p.line).Children(func() {
		ui.Column(c).Grow(1).Width(0).MinWidth(0).Gap(4).Children(func() {
			ui.Text(c, title).FontSize(13).FontWeight(500).TextColor(p.text)
			if hint != "" {
				ui.Text(c, hint).FontSize(12).LineHeight(1.45).TextColor(p.muted)
			}
		})
		if control != nil {
			ui.Row(c).Shrink(0).Gap(10).AlignItems(ui.Center).Children(control)
		}
	})
}

// remark is a muted paragraph under the settings of a pane.
func remark(c *ui.Context, p palette, text string) {
	ui.Text(c, text).FontSize(12).LineHeight(1.45).TextColor(p.muted)
}

// value is read-only text on the right of a setting.
func value(c *ui.Context, p palette, text string) {
	ui.Text(c, text).Font(mono).FontSize(12).FontFeatures("tnum").TextColor(p.text).Selectable()
}

// choice is a segmented control drawn like the list filters: a soft track
// whose chosen segment is a raised chip. It reports a new choice.
func choice(c *ui.Context, p palette, label string, selected *int, labels ...string) bool {
	return filters(c, p, label, selected, labels, nil)
}

// pick is a drop-down over options showing current; it returns the option
// chosen this frame, if one was.
func pick(c *ui.Context, label, current string, options []string) (string, bool) {
	chosen := current
	changed := ui.Select(c, &chosen, options).Label(label).Height(26).MinWidth(170).Cursor(ui.CursorPointer).Changed()
	return chosen, changed && chosen != current
}

// appearancePane holds what only changes how this window looks. Every
// choice applies at once and is kept for the next launch.
func (s *desktop) appearancePane(c *ui.Context, p palette) {
	heading(c, p, tr("外观", "Appearance"))

	system := tr("跟随系统", "Follow system")
	options := []string{system}
	for _, language := range languages {
		options = append(options, language.name)
	}
	now := languageName(prefs{}.language(s.locale))
	current := system
	if s.prefs.Lang != "" && s.prefs.Lang != "system" {
		current = languageName(s.prefs.language(s.locale))
	}
	pref(c, p, tr("语言", "Language"), fmt.Sprintf(tr("跟随 %s（当前为%s），或始终使用一种语言。", "Follow %s (now %s), or always use one language."), osName(), now), func() {
		if chosen, ok := pick(c.Key("lang"), tr("语言", "Language"), current, options); ok {
			s.prefs.Lang = "system"
			for _, language := range languages {
				if language.name == chosen {
					s.prefs.Lang = language.tag
				}
			}
			s.savePrefs()
			if s.window != nil {
				s.applyLanguage()
			}
		}
	})

	themes := []string{"system", "dark", "light"}
	ui.Column(c.Key("theme")).FillWidth().Padding(0, 0, 18).Gap(14).BorderWidth(0, 0, 1, 0).BorderColor(p.line).Children(func() {
		ui.Row(c).FillWidth().Gap(24).AlignItems(ui.Center).Children(func() {
			ui.Column(c).Grow(1).Width(0).MinWidth(0).Gap(4).Children(func() {
				ui.Text(c, tr("主题", "Theme")).FontSize(13).FontWeight(600).TextColor(p.text)
				remark(c, p, fmt.Sprintf(tr("跟随 %s，或始终使用深色或浅色主题。", "Follow %s, or always use the dark or light theme."), osName()))
			})
			idx := max(0, indexOf(themes, s.prefs.Theme))
			if choice(c.Key("theme-seg"), p, tr("主题", "Theme"), &idx, tr("跟随系统", "System"), tr("深色", "Dark"), tr("浅色", "Light")) {
				s.setTheme(themes[idx])
			}
		})
		ui.Row(c).Gap(12).Children(func() {
			for _, dark := range []bool{true, false} {
				if s.themeCard(c.Key(dark), p, dark) {
					s.setTheme(map[bool]string{true: "dark", false: "light"}[dark])
				}
			}
		})
	})

	pref(c, p, tr("强调色", "Accent"), tr("选中项、焦点环和主要按钮。", "Selection, focus rings and primary buttons."), func() {
		for _, a := range accents {
			on := a.id == s.prefs.Accent || (s.prefs.Accent == "" && a.id == accents[0].id)
			b := clickable(c.Key(a.id)).Label(tr(a.zh, a.en)).Tooltip(tr(a.zh, a.en)).Size(26, 26).Radius(13).Center()
			if on {
				b.Border(2, p.text)
			}
			b.Children(func() { ui.Box(c).Size(18, 18).Radius(9).Background(accentColor(a.id, s.dark)) })
			if b.Clicked() {
				s.prefs.Accent = a.id
				s.savePrefs()
			}
		}
	})

	pref(c, p, tr("终端背景", "Terminal background"), tr("容器、Linux 机器和宿主的终端。", "Container, Linux machine and host terminals."), func() {
		for _, b := range termBackgrounds {
			on := b.id == s.prefs.TermBg || (s.prefs.TermBg == "" && b.id == termBackgrounds[0].id)
			sw := clickable(c.Key("term-"+b.id)).Label(tr(b.zh, b.en)).Tooltip(tr(b.zh, b.en)).Size(26, 26).Radius(13).Center()
			if on {
				sw.Border(2, p.accent)
			}
			sw.Children(func() {
				dot := ui.Box(c).Size(18, 18).Radius(9).Border(1, p.line)
				if b.color == "" {
					// Following the appearance: half white, half black.
					dot.Gradient(ui.Hex("#FFFFFF"), ui.Hex("#000000"), 90)
				} else {
					dot.Background(ui.Hex(b.color))
				}
			})
			if sw.Clicked() {
				s.prefs.TermBg = b.id
				s.savePrefs()
				s.retheme()
			}
		}
	})

	// Listing the system's fonts takes a moment the first time, so it waits
	// until this pane is shown.
	if s.fonts == nil {
		for _, f := range ui.FontFamilies() {
			s.fonts = append(s.fonts, f.Name)
			if f.Monospaced {
				s.monoFonts = append(s.monoFonts, f.Name)
			}
		}
	}
	font := func(key, title, hint string, family *string, families []string) {
		standard := tr("系统默认", "System default")
		current := standard
		if *family != "" {
			current = *family
		}
		pref(c, p, title, hint, func() {
			if chosen, ok := pick(c.Key(key), title, current, append([]string{standard}, families...)); ok {
				if *family = chosen; chosen == standard {
					*family = ""
				}
				s.savePrefs()
			}
		})
	}
	font("font", tr("界面字体", "Interface font"), tr("表格、侧边栏和标题。", "Tables, the sidebar and headings."), &s.prefs.Font, s.fonts)
	font("mono", tr("等宽字体", "Monospace font"), tr("ID、命令和代码。", "IDs, commands and code."), &s.prefs.Mono, s.monoFonts)

	pref(c, p, tr("文字大小", "Text size"), tr("窗口里的所有内容。", "Everything in the window."), func() {
		idx := nearest(textScales, s.prefs.Scale)
		if choice(c.Key("scale"), p, tr("文字大小", "Text size"), &idx, "90%", "100%", "110%", "125%") {
			s.prefs.Scale = textScales[idx]
			s.savePrefs()
		}
	})
	pref(c, p, tr("日志文字", "Log text"), tr("容器日志和终端输出。", "Container logs and terminal output."), func() {
		idx := nearest(logSizes, s.prefs.LogSize)
		if choice(c.Key("log-size"), p, tr("日志文字", "Log text"), &idx, tr("小", "Small"), tr("中", "Medium"), tr("大", "Large")) {
			s.prefs.LogSize = logSizes[idx]
			s.savePrefs()
		}
	})
	pref(c, p, tr("列表密度", "List density"), tr("表格每一行的高度。", "The height of every table row."), func() {
		idx := 0
		if s.prefs.Density == "cozy" {
			idx = 1
		}
		if choice(c.Key("density"), p, tr("列表密度", "List density"), &idx, tr("紧凑", "Compact"), tr("舒适", "Comfortable")) {
			s.prefs.Density = []string{"compact", "cozy"}[idx]
			s.savePrefs()
		}
	})

	ui.Column(c.Key("preview")).FillWidth().Padding(12, 14).Gap(8).Radius(8).Background(p.panel).Border(1, p.line).Children(func() {
		ui.Text(c, tr("预览", "Preview")).FontSize(11).FontWeight(600).LetterSpacing(0.5).TextColor(p.muted)
		ui.Row(c).Gap(10).AlignItems(ui.Center).Children(func() {
			dot(c, p, "running")
			ui.Text(c, "shop-api-1").FontSize(13).FontWeight(600).TextColor(p.text)
			ui.Text(c, "shop/api:dev").Font(mono).FontSize(12).TextColor(p.muted)
			button(c.Key("sample"), p, tr("主要按钮", "Primary"), "", primaryButton, false)
		})
		ui.Text(c, "GET /api/products 200 12ms").Font(mono).FontSize(s.prefs.LogSize).TextColor(p.text)
	})
}

// indexOf is the position of v in list, or -1.
func indexOf(list []string, v string) int {
	for i, item := range list {
		if item == v {
			return i
		}
	}
	return -1
}

// setTheme chooses system, dark or light and tells the windows.
func (s *desktop) setTheme(theme string) {
	s.prefs.Theme = theme
	// Without a window, as in tests, there is no chrome to restyle.
	if s.window != nil {
		s.applyTheme()
	}
	s.savePrefs()
}

// themeCard is a small picture of the window in one appearance. A click
// pins that appearance; the one on screen is marked as in use.
func (s *desktop) themeCard(c *ui.Context, p palette, dark bool) bool {
	t := colors(dark, s.prefs.Accent)
	name := tr("浅色", "Light")
	if dark {
		name = tr("深色", "Dark")
	}
	b := clickable(c).Label(tr("使用", "Use ")+name).Width(168).Radius(8).Background(p.panel).Border(1, p.line)
	if dark == s.dark {
		b.Border(1, p.accent)
	}
	b.Children(func() {
		ui.Column(c).FillWidth().Padding(8).Gap(8).Children(func() {
			ui.Row(c).FillWidth().Height(74).Radius(5).Background(t.bg).Border(1, t.line).Children(func() {
				ui.Column(c).Width(40).FillHeight().Padding(8, 6).Gap(5).Background(t.panel).Children(func() {
					ui.Box(c).FillWidth().Height(5).Radius(2).Background(t.accent)
					ui.Box(c).FillWidth().Height(5).Radius(2).Background(t.raised)
					ui.Box(c).FillWidth().Height(5).Radius(2).Background(t.raised)
				})
				ui.Column(c).Grow(1).Padding(10, 9).Gap(6).Children(func() {
					ui.Box(c).Width(54).Height(5).Radius(2).Background(t.text)
					ui.Box(c).FillWidth().Height(5).Radius(2).Background(t.muted.Alpha(0.5))
					ui.Box(c).Width(70).Height(5).Radius(2).Background(t.muted.Alpha(0.5))
					ui.Box(c).Width(34).Height(10).Radius(3).Background(t.accent)
				})
			})
			ui.Row(c).FillWidth().Justify(ui.SpaceBetween).AlignItems(ui.Center).Children(func() {
				ui.Text(c, name).FontSize(12).FontWeight(500).TextColor(p.text)
				if dark == s.dark {
					ui.Text(c, tr("使用中", "In use")).FontSize(11).TextColor(p.accent)
				}
			})
		})
	})
	return b.Clicked()
}

// stopEngine asks before turning the engine off, which stops every container.
func (s *desktop) stopEngine() {
	s.pending = &confirmation{
		title: tr("停止引擎？", "Stop the engine?"),
		body:  tr("运行中的容器会停止，引擎在你再次启动前保持关闭。", "Running containers stop, and the engine stays off until you start it again."),
		ok:    tr("停止", "Stop"),
		run:   func() { s.do("engine-stop", engineAction{http.MethodPost, "/fabo/stop", nil}, "") },
	}
}

// restartVM puts the VM to sleep and wakes it, which is how new CPU and
// memory limits take effect. The wake is skipped when the sleep fails.
func (s *desktop) restartVM() {
	s.working["engine"] = true
	s.run(2*time.Minute, func(ctx context.Context) func() {
		err := s.engine.request(ctx, http.MethodPost, "/fabo/sleep", nil, nil)
		if err == nil {
			err = s.engine.request(ctx, http.MethodPost, "/fabo/wake", nil, nil)
		}
		return func() {
			delete(s.working, "engine")
			s.settle(err, tr("虚拟机已重启", "VM restarted"))
		}
	})
}

// resourcesPane switches the engine on and off and sizes its VM.
func (s *desktop) resourcesPane(c *ui.Context, p palette) {
	heading(c, p, tr("Linux 虚拟机", "Linux VM"))
	if !s.connected {
		remark(c, p, tr("引擎未连接，无法读取资源设置。", "The engine is not reachable, so its resources cannot be read."))
		return
	}
	if s.info.FaboVMControls == nil || *s.info.FaboVMControls {
		state := s.engineState()
		title, hint := tr("运行中", "Running"), tr("没有任务时会自行休眠，用到时再唤醒。", "It rests by itself when nothing runs and wakes when you use it.")
		switch state {
		case "stopped":
			title, hint = tr("已关闭", "Off"), tr("点「启动引擎」来运行容器。重启电脑后它仍保持关闭，直到你启动它。", "Press Start engine to run containers. It stays off after a restart until you start it.")
		case "starting":
			title = tr("启动中…", "Starting…")
		case "stopping":
			title = tr("停止中…", "Stopping…")
		case "asleep":
			title = tr("休眠中", "Asleep")
		}
		pref(c, p, title, hint, func() {
			if state == "stopped" {
				if button(c.Key("on"), p, tr("启动引擎", "Start engine"), "play", primaryButton, s.working["engine"]) {
					s.do("engine", engineAction{http.MethodPost, "/fabo/wake", nil}, tr("引擎已启动", "Engine started"))
				}
			} else if button(c.Key("off"), p, tr("关闭引擎", "Turn off engine"), "power", plainButton, s.working["engine"] || s.working["engine-stop"]) {
				s.stopEngine()
			}
		})
	}
	if !s.resOK {
		remark(c, p, tr("这个引擎直接运行在 Linux 上，没有需要分配资源的虚拟机。", "This engine runs directly on Linux and has no VM to size."))
		return
	}
	r := s.res
	pref(c, p, tr("CPU", "CPUs"), fmt.Sprintf(tr("与 %s 共用：只有容器忙的时候虚拟机才占用它们。", "Shared with %s: the VM only uses them while containers are busy."), osName()), func() {
		ui.StepSlider(c.Key("cpus"), &s.cpus, 1, float64(max(1, r.MaxCPUs)), 1).Label(tr("CPU", "CPUs")).Width(200).Cursor(ui.CursorPointer)
		ui.Text(c, fmt.Sprintf(tr("%.0f 核", "%.0f"), s.cpus)).Width(52).Font(mono).FontSize(12).FontFeatures("tnum").TextColor(p.text)
	})
	pref(c, p, tr("内存", "Memory"), fmt.Sprintf(tr("虚拟机最多可用的内存。容器需要多少才用多少，其余还给 %s。", "The most the VM may take. It uses memory only as containers need it and gives the rest back to %s."), osName()), func() {
		ui.StepSlider(c.Key("memory"), &s.memory, 1, float64(max(1, r.MaxMemory/1024)), 1).Label(tr("内存", "Memory")).Width(200).Cursor(ui.CursorPointer)
		ui.Text(c, fmt.Sprintf("%.0f GB", s.memory)).Width(52).Font(mono).FontSize(12).FontFeatures("tnum").TextColor(p.text)
	})

	cpus, memory := int(s.cpus), int(s.memory)*1024
	changed := cpus != r.CPUs || memory != r.Memory
	// A running VM keeps the size it started with, so saved values that
	// differ from the running ones wait for a restart.
	stale := r.Awake && (r.RunningCPUs != r.CPUs || r.RunningMemory != r.Memory)
	ui.Row(c.Key("apply")).FillWidth().Gap(10).AlignItems(ui.Center).Children(func() {
		running := tr("虚拟机在休眠，下次启动时使用这些设置。", "The VM is asleep; it starts with these next time.")
		if r.Awake {
			running = fmt.Sprintf(tr("当前以 %d 核、最多 %.0f GB 运行。", "Running now with %d CPUs and up to %.0f GB."), r.RunningCPUs, float64(r.RunningMemory)/1024)
		}
		ui.Text(c, running).Grow(1).Width(0).MinWidth(0).FontSize(12).TextColor(p.muted)
		if stale && !changed && button(c.Key("restart"), p, tr("重启虚拟机以生效", "Restart VM to apply"), "restart", plainButton, s.working["engine"]) {
			s.pending = &confirmation{
				title: tr("重启虚拟机？", "Restart the VM?"),
				body:  tr("重启会停止运行中的容器，之后需要重新启动它们。", "Restarting stops the running containers; start them again afterwards."),
				ok:    tr("重启", "Restart"), run: s.restartVM,
			}
		}
		if button(c.Key("save"), p, tr("保存", "Save"), "", primaryButton, !changed || s.working["resources"]) {
			s.do("resources", engineAction{http.MethodPost, "/fabo/resources", map[string]int{"cpus": cpus, "memory_mib": memory}},
				tr("已保存，虚拟机下次启动时生效", "Saved. The VM uses these from its next start"))
		}
	})
}

// networkPane sets the proxy image pulls and pushes go through.
func (s *desktop) networkPane(c *ui.Context, p palette) {
	heading(c, p, tr("代理", "Proxy"))
	if !s.proxyOK {
		remark(c, p, tr("启动引擎后才能修改代理。", "Start the engine to change the proxy."))
		return
	}
	d, eff := &s.proxyDraft, s.proxy.Effective
	use := tr("当前没有使用代理。", "No proxy is in use.")
	if proxy := eff.HTTPS; proxy != "" || eff.HTTP != "" {
		if proxy == "" {
			proxy = eff.HTTP
		}
		use = fmt.Sprintf(tr("正在使用：%s。", "In use: %s."), proxy)
		if len(eff.NoProxy) > 0 {
			use = fmt.Sprintf(tr("正在使用：%s，%s 除外。", "In use: %s, except %s."), proxy, strings.Join(eff.NoProxy, ", "))
		}
	}
	modes := []string{"system", "custom", "none"}
	pref(c, p, tr("代理", "Proxy"), tr("用于拉取和推送镜像。", "For pulling and pushing images.")+" "+use, func() {
		idx := max(0, indexOf(modes, d.Mode))
		if choice(c.Key("mode"), p, tr("代理", "Proxy"), &idx, fmt.Sprintf(tr("%s 设置", "%s settings"), osName()), tr("自定义", "Custom"), tr("不使用", "None")) {
			d.Mode = modes[idx]
		}
	})
	if d.Mode == "system" && eff.PAC {
		remark(c, p, fmt.Sprintf(tr("%s 使用的是自动代理配置（PAC）文件，Fabo 无法跟随。请选「自定义」并填写代理。", "%s uses an automatic proxy configuration (PAC) file, which Fabo can't follow. Choose Custom and enter the proxy."), osName()))
	}
	if d.Mode == "custom" {
		field := func(key, title, hint string, value *string) {
			pref(c, p, title, "", func() {
				ui.TextInput(c.Key(key), value).Label(title).Placeholder(hint).Width(280).Height(28).Font(mono).FontSize(12)
			})
		}
		field("http", tr("HTTP 代理", "HTTP proxy"), "proxy.example.com:3128", &d.HTTP)
		field("https", tr("HTTPS 代理", "HTTPS proxy"), "proxy.example.com:3128", &d.HTTPS)
		field("bypass", tr("不走代理", "Bypass for"), "*.internal, registry.corp", &d.NoProxy)
	}
	pref(c, p, tr("传给容器", "Pass to containers"), tr("新容器会带上 HTTP_PROXY、HTTPS_PROXY 和 NO_PROXY。本机上的代理以 host.docker.internal 访问。", "New containers get HTTP_PROXY, HTTPS_PROXY and NO_PROXY. A proxy on this Mac is reached as host.docker.internal."), func() {
		idx := 0
		if d.Containers {
			idx = 1
		}
		if choice(c.Key("containers"), p, tr("传给容器", "Pass to containers"), &idx, tr("关", "Off"), tr("开", "On")) {
			d.Containers = idx == 1
		}
	})
	ui.Row(c.Key("apply")).FillWidth().Gap(10).AlignItems(ui.Center).Children(func() {
		ui.Text(c, fmt.Sprintf(tr("VPN 不需要设置：容器流量经由 %s 发出。", "VPNs need no setting: container traffic leaves through %s."), osName())).
			Grow(1).Width(0).MinWidth(0).FontSize(12).TextColor(p.muted)
		if button(c.Key("save"), p, tr("保存", "Save"), "", primaryButton, *d == s.proxy.Settings || s.working["proxy"]) {
			s.do("proxy", engineAction{http.MethodPost, "/fabo/proxy", *d}, tr("代理设置已保存", "Proxy settings saved"))
		}
	})
}

// builderRows are the builder's state and cache, shown by the Builds page
// and by Settings → Builds.
func (s *desktop) builderRows(c *ui.Context, p palette) {
	b := s.builder
	pref(c, p, tr("Fabo 的构建器", "Fabo's builder"), b.Image, func() {
		kind, text := "stopped", tr("未运行", "Not running")
		if b.State == "running" {
			kind, text = "running", tr("运行中", "Running")
		} else if b.State != "" && b.State != "missing" {
			text = b.State
		}
		dot(c, p, kind)
		ui.Text(c, text).FontSize(12).TextColor(stateColor(p, kind))
	})
	pref(c, p, tr("缓存上限", "Cache limit"), fmt.Sprintf(tr("构建缓存超过它时，最旧的部分会被清掉。默认 %.0f GB。", "The oldest cache goes once it grows past this. Default %.0f GB."), b.DefaultCacheGB), func() {
		ui.TextInput(c.Key("cache"), &s.cacheGB).Label(tr("缓存上限", "Cache limit")).Width(72).Height(28).Font(mono).FontSize(12)
		ui.Text(c, "GB").FontSize(12).TextColor(p.muted)
		gb, err := strconv.ParseFloat(strings.TrimSpace(s.cacheGB), 64)
		if button(c.Key("save-cache"), p, tr("保存", "Save"), "", plainButton, err != nil || gb <= 0 || gb == b.CacheGB) {
			s.do("builder", engineAction{http.MethodPost, "/fabo/builder", map[string]any{"cache_gb": gb, "idle_secs": b.IdleSecs}}, tr("已保存", "Saved"))
		}
	})
	pref(c, p, tr("空闲后停止", "Stops when idle"), tr("没有构建时构建器自己停下，下次构建再启动。", "The builder stops by itself between builds and starts with the next one."), func() {
		value(c, p, ago(int64(b.IdleSecs)))
	})
	pref(c, p, tr("构建缓存", "Build cache"), tr("清理后，下一次构建会重新执行所有步骤。", "After clearing, the next build runs every step again."), func() {
		if s.disk != nil {
			value(c, p, displaySize(s.disk.buildCache))
		}
		if button(c.Key("prune-cache"), p, tr("清理构建缓存", "Clear build cache"), "", plainButton, s.working["builder"]) {
			s.pending = &confirmation{
				title: tr("清理构建缓存？", "Clear the build cache?"),
				body:  tr("下一次构建会重新执行所有步骤。", "The next build runs every step again."), ok: tr("清理", "Clear"),
				run: func() {
					s.do("builder", engineAction{http.MethodPost, "/build/prune?all=1", nil}, tr("构建缓存已清理", "Build cache cleared"))
				},
			}
		}
	})
}

// setNames asks the launcher to switch *.fabo.local names ("local") or the
// trust in the engine's certificate ("https"). macOS asks for a password on
// the way; closing that prompt is a choice and is not reported.
func (s *desktop) setNames(kind string, on bool) {
	done, failed := tr("容器现在通过 <name>.fabo.localhost 访问", "Containers answer at <name>.fabo.localhost"), tr("无法更改容器域名：", "Could not change container names: ")
	switch {
	case kind == "https" && on:
		done = tr("已信任 Fabo 的证书：容器可通过 HTTPS 打开", "Fabo's certificate is trusted: containers open over HTTPS")
	case kind == "https":
		done = tr("已不再信任 Fabo 的证书", "Fabo's certificate is no longer trusted")
	case on:
		done = tr("容器现在通过 <name>.fabo.local 访问", "Containers now answer at <name>.fabo.local")
	}
	if kind == "https" {
		failed = tr("无法更改证书的信任设置：", "Could not change the certificate's trust: ")
	}
	state := "off"
	if on {
		state = "on"
	}
	launcher := s.launcher
	s.working["names"] = true
	s.run(5*time.Minute, func(ctx context.Context) func() {
		_, err := launcher(ctx, "domains", kind, state)
		return func() {
			delete(s.working, "names")
			switch {
			case err == nil:
				s.settle(nil, done)
			case strings.Contains(err.Error(), "canceled"):
				s.settle(nil, "")
			default:
				s.settle(errors.New(failed+err.Error()), "")
			}
		}
	})
}

// domainsPane is Settings → Domains: which suffix container names carry,
// whether they open over HTTPS, and the ports they answer on. The first two
// are the launcher's to change; without one (Linux, or a window started on
// its own) names are *.fabo.localhost and there is nothing to switch.
func (s *desktop) domainsPane(c *ui.Context, p palette) {
	heading(c, p, tr("域名", "Domains"))
	suffix := "fabo.localhost"
	if !s.namesOK {
		pref(c, p, tr("本地域名", "Local names"), tr("运行中的容器通过 <name>.fabo.localhost 访问，无需任何设置。", "Running containers answer at <name>.fabo.localhost, which needs no setup."), nil)
	} else {
		suffix = s.names.Suffix
		hint := tr("关闭：容器通过 <name>.fabo.localhost 访问，无需任何设置。", "Off: containers answer at <name>.fabo.localhost, which needs no setup.")
		if s.names.On {
			hint = tr("开启：运行中的容器可通过 <name>.fabo.local 访问。", "On: running containers answer at <name>.fabo.local.")
		}
		pref(c, p, tr("本地域名", "Local names"), hint, func() {
			idx := 0
			if s.names.On {
				idx = 1
			}
			// The choice shows what the machine has, so it moves only
			// once the launcher has made the change.
			if choice(c.Key("names"), p, tr("本地域名", "Local names"), &idx, "fabo.localhost", "fabo.local") && !s.working["names"] {
				s.setNames("local", idx == 1)
			}
		})
		hint = fmt.Sprintf(tr("信任 Fabo 的证书，让 https://myapp.%s 可用。%s 会询问你的密码。该证书只能为 Fabo 自己的域名担保。",
			"Trust Fabo's certificate so https://myapp.%s works. %s asks for your password. The certificate can only vouch for Fabo's own names."), suffix, osName())
		if s.names.HTTPS {
			hint = tr("开启：容器以 https:// 打开，使用 Fabo 自己的证书，该证书只为 .fabo.local 和 .fabo.localhost 域名担保。",
				"On: containers open at https:// with Fabo's own certificate, which only vouches for .fabo.local and .fabo.localhost names.")
		}
		pref(c, p, "HTTPS", hint, func() {
			idx := 0
			if s.names.HTTPS {
				idx = 1
			}
			if choice(c.Key("https"), p, "HTTPS", &idx, tr("关闭", "Off"), tr("开启", "On")) && !s.working["names"] {
				s.setNames("https", idx == 1)
			}
		})
	}
	s.domainPorts(c, p, suffix)
	about := fmt.Sprintf(tr("Compose 服务可通过 <service>.<project>.%s 访问。", "Compose services answer at <service>.<project>.%s."), suffix)
	if s.namesOK {
		about += " " + tr("更改此项需要管理员权限（管理员密码），因为它会添加或删除 /etc/resolver/fabo.local。",
			"Changing this needs admin rights (an administrator password), since it adds or removes /etc/resolver/fabo.local.")
	}
	remark(c, p, about)
}

// domainPorts changes the ports container names answer on.
func (s *desktop) domainPorts(c *ui.Context, p palette, suffix string) {
	if !s.domainsOK {
		pref(c, p, tr("端口", "Ports"), tr("启动 Fabo 以更改域名应答的端口。", "Start Fabo to change the ports names answer on."), nil)
		return
	}
	d := s.domains
	// The example link uses a port the names are not already on.
	example := 8080
	if d.HTTP.Port == 8080 {
		example = 8081
	}
	hint := fmt.Sprintf(tr("域名在 %d 和 %d 端口应答。别的程序要用这两个端口时可以换；换了之后链接会带上端口，比如 http://web.%s:%d。",
		"Names answer on %d and %d. Pick other ports when another app needs these; links then carry the port, like http://web.%s:%d."), d.HTTP.Port, d.HTTPS.Port, suffix, example)
	ui.Column(c.Key("ports")).FillWidth().Padding(0, 0, 18).Gap(12).BorderWidth(0, 0, 1, 0).BorderColor(p.line).Children(func() {
		ui.Text(c, tr("端口", "Ports")).FontSize(13).FontWeight(600).TextColor(p.text)
		remark(c, p, hint)
		ui.Row(c).Gap(10).AlignItems(ui.End).Children(func() {
			port := func(key, title string, value *string, fixed bool) {
				ui.Column(c.Key(key)).Gap(4).Children(func() {
					ui.Text(c, title).FontSize(11).TextColor(p.muted)
					ui.TextInput(c.Key("in"), value).Label(tr(title+" 端口", title+" port")).Width(92).Height(28).Font(mono).FontSize(12).Disabled(fixed)
				})
			}
			port("http", "HTTP", &s.portHTTP, d.HTTP.Env != nil)
			port("https", "HTTPS", &s.portHTTPS, d.HTTPS.Env != nil)
			h, e1 := strconv.Atoi(strings.TrimSpace(s.portHTTP))
			t, e2 := strconv.Atoi(strings.TrimSpace(s.portHTTPS))
			valid := e1 == nil && e2 == nil && h > 0 && h < 65536 && t > 0 && t < 65536 && h != t
			same := h == d.HTTP.Port && t == d.HTTPS.Port
			if button(c.Key("change"), p, tr("更改端口", "Change ports"), "", primaryButton, !valid || same || s.working["domains"]) {
				s.do("domains", engineAction{http.MethodPost, "/fabo/domains", map[string]int{"http": h, "https": t}}, tr("端口已更改", "Ports changed"))
			}
		})
	})
	var notes []string
	for _, k := range []struct {
		kind string
		port domainPort
	}{{"HTTP", d.HTTP}, {"HTTPS", d.HTTPS}} {
		if k.port.Env != nil {
			notes = append(notes, fmt.Sprintf(tr("%s 固定了 %s 端口，这里改不了。", "%s sets the %s port, so it can't be changed here."), *k.port.Env, k.kind))
		}
		if !k.port.Running {
			notes = append(notes, fmt.Sprintf(tr("%s 域名未启用：端口 %d 被占用，请换一个。", "%s names are off: port %d is in use. Pick another."), k.kind, k.port.Port))
		} else if !k.port.Serving {
			notes = append(notes, fmt.Sprintf(tr("%s 域名已暂停：有容器占用了端口 %d。", "%s names are paused: a container has port %d."), k.kind, k.port.Port))
		}
	}
	notes = append(notes, tr("容器发布了其中某个端口时由容器使用，该端口上的域名会暂停，直到容器让出。", "A container that publishes one of these ports gets it; names on that port pause until it lets go."))
	remark(c, p, strings.Join(notes, " "))
}

// aboutPane says what is running.
func (s *desktop) aboutPane(c *ui.Context, p palette) {
	ui.Row(c.Key("brand")).FillWidth().Gap(14).AlignItems(ui.Center).Padding(0, 0, 18).BorderWidth(0, 0, 1, 0).BorderColor(p.line).Children(func() {
		ui.Image(c, markIcon).Size(52, 52)
		ui.Column(c).Gap(4).Children(func() {
			ui.Text(c, "Fabo").FontSize(20).FontWeight(700).TextColor(p.text)
			remark(c, p, tr("小而快的容器运行时。", "A small, fast container runtime."))
		})
	})
	engine, vm := tr("未运行", "Not running"), ""
	if s.connected {
		engine = "Fabo " + s.version.Version + " · Docker API " + s.version.ApiVersion
		switch s.engineState() {
		case "running":
			vm = tr("运行中", "Running")
		case "asleep":
			vm = tr("休眠中", "Asleep")
		case "stopped":
			vm = tr("已关闭", "Off")
		case "starting":
			vm = tr("启动中…", "Starting…")
		}
		if vm != "" && s.version.Os != "" {
			vm += " · " + s.version.Os + "/" + s.version.Arch
		}
	}
	rows := []field{{tr("引擎", "Engine"), engine}, {tr("虚拟机", "VM"), vm}}
	if s.connected && s.resOK {
		rows = append(rows, field{tr("CPU", "CPUs"), strconv.Itoa(s.res.CPUs)},
			field{tr("内存", "Memory"), fmt.Sprintf(tr("最多 %.0f GB", "up to %.0f GB"), float64(s.res.Memory)/1024)})
	}
	rows = append(rows, field{"Docker context", s.contextName}, field{"Go", s.version.GoVersion})
	ui.Column(c.Key("facts")).FillWidth().Gap(9).Children(func() {
		for i, f := range compact(rows) {
			ui.Row(c.Key(i)).FillWidth().Gap(16).Children(func() {
				ui.Text(c, f.label).Width(130).Shrink(0).FontSize(12.5).TextColor(p.muted)
				ui.Text(c, f.value).Grow(1).Width(0).MinWidth(0).Font(mono).FontSize(12).TextColor(p.text).Selectable()
			})
		}
	})
}
