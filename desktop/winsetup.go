package main

import (
	"context"
	"fmt"
	"time"

	"github.com/egoist/mygo/ui"
)

// winState is what the window knows about fabo-win on Windows.
type winState struct {
	status  hostStatus
	setup   hostSetup
	update  hostUpdate
	known   bool // a status has been read
	polling bool
	// linux is the choice on the "choose" step: 0 Fabo Linux, 1 Ubuntu.
	linux int
	// acting is the setup action in flight, so its button waits.
	acting string
	// updateLater hides the update strip until the window opens again.
	updateLater bool
	// build is which builder docker build uses (winbuilds.go); buildKnown
	// once read, buildAsked once the window's question was answered, and
	// buildActing while a choice is on its way.
	build                               hostBuildWith
	buildKnown, buildAsked, buildActing bool
	// resErr is why WSL's resources could not be read from .wslconfig.
	resErr string
	// reportContainers: the diagnostic report lists the containers too.
	reportContainers bool
	// reportPath is where the last report was saved, as fabo-win wrote it.
	reportPath string
}

// setupSteps are the rail's steps, with the host's step names each covers.
var setupSteps = []struct {
	steps  []string
	zh, en string
}{
	{[]string{"welcome", "wsl", "restart"}, "WSL", "WSL"},
	{[]string{"choose", "linux"}, "Linux", "Linux"},
	{[]string{"engine"}, "引擎", "Engine"},
	{[]string{"tools"}, "Docker 工具", "Docker tools"},
	{[]string{"ready"}, "就绪", "Ready"},
}

// refreshHost reads fabo-win's status, and setup's while it is pending.
// Setup polls faster, so its progress moves.
func (s *desktop) refreshHost() {
	if s.host == nil || s.win.polling {
		return
	}
	// In the tray with setup done, nothing on screen needs fabo-win's state;
	// each read would also ping the engine through the bridge.
	if s.window != nil && !s.window.IsVisible() && s.win.known && !s.win.status.SetupPending {
		return
	}
	s.win.polling = true
	host := s.host
	buildKnown, onSettings := s.win.buildKnown, s.page == settingsPage
	s.run(5*time.Second, func(ctx context.Context) func() {
		st, err := host.status(ctx)
		var setup hostSetup
		var upd hostUpdate
		if err == nil && st.SetupPending {
			setup, _ = host.setup(ctx)
		}
		if err == nil && !st.SetupPending {
			upd, _ = host.update(ctx)
		}
		// Read once for the window's question, and while Settings shows it.
		var build hostBuildWith
		buildErr := err
		if err == nil && !st.SetupPending && (!buildKnown || onSettings) {
			build, buildErr = host.buildWith(ctx)
		} else if buildErr == nil {
			buildErr = errSkipped
		}
		return func() {
			s.win.polling = false
			if err != nil {
				return
			}
			was := s.win.status.SetupPending
			s.win.status, s.win.known, s.win.update = st, true, upd
			// A choice on its way is shown as made until it lands.
			if buildErr == nil && !s.win.buildActing {
				s.win.build, s.win.buildKnown = build, true
			}
			if st.SetupPending {
				s.win.setup = setup
			}
			if was && !st.SetupPending {
				// Setup has finished: the containers appear.
				s.refreshing = false
				s.refresh()
			}
		}
	})
}

// setupAction sends one setup action and reads setup's state again.
func (s *desktop) setupAction(action string, value any) {
	if s.host == nil || s.win.acting != "" {
		return
	}
	s.win.acting = action
	host := s.host
	s.run(30*time.Second, func(ctx context.Context) func() {
		err := host.setupAction(ctx, action, value)
		setup, serr := host.setup(ctx)
		st, sterr := host.status(ctx)
		return func() {
			s.win.acting = ""
			if err != nil {
				s.notices = append(s.notices, err.Error())
			}
			if serr == nil {
				s.win.setup = setup
			}
			if sterr == nil {
				was := s.win.status.SetupPending
				s.win.status = st
				if was && !st.SetupPending {
					s.refreshing = false
					s.refresh()
				}
			}
		}
	})
}

// setupPage is the whole window while first-run setup is pending: a rail
// of steps beside the current one.
func (s *desktop) setupPage(c *ui.Context, p palette) {
	v := s.win.setup
	ui.Row(c.Key("setup")).Fill().Background(p.bg).AlignItems(ui.Stretch).Children(func() {
		ui.Column(c.Key("setup-rail")).Width(220).Shrink(0).Padding(56, 20, 20, 28).Gap(14).Children(func() {
			ui.Text(c, "Fabo").FontSize(20).FontWeight(700).TextColor(p.text)
			ui.Box(c).Height(10)
			current := setupRailIndex(v.Step)
			for i, step := range setupSteps {
				ink, mark := p.muted, ""
				switch {
				case i < current:
					ink, mark = p.sub, "check"
				case i == current:
					ink = p.text
				}
				ui.Row(c.Key(fmt.Sprintf("rail-%d", i))).Gap(10).Children(func() {
					circle := ui.Box(c).Size(20, 20).Radius(10).Center()
					if i == current {
						circle.Background(p.accent)
					} else {
						circle.Border(1.5, ink.Alpha(0.6))
					}
					circle.Children(func() {
						if mark != "" {
							ui.Icon(c, icons["check"]).Size(12, 12).TextColor(ink)
						} else {
							num := ui.Textf(c, "%d", i+1).FontSize(11).FontWeight(600).TextColor(ink)
							if i == current {
								num.TextColor(p.onAccent)
							}
						}
					})
					ui.Text(c, tr(step.zh, step.en)).FontSize(13).FontWeight(500).TextColor(ink)
				})
			}
		})
		ui.Column(c.Key("setup-card")).Grow(1).Width(0).MinWidth(0).Margin(8, 8, 8, 0).Radius(14).
			Background(p.panel).Border(1, p.line).Clip().Children(func() {
			ui.Scroll(c.Key("setup-scroll")).Grow(1).MinHeight(0).Children(func() {
				ui.Column(c.Key("setup-body")).MaxWidth(620).Padding(48, 40, 32, 40).Gap(16).Children(func() {
					if !s.win.known || v.Step == "" {
						ui.Spinner(c)
						return
					}
					s.setupStep(c, p, v)
				})
			})
		})
	})
}

// setupRailIndex is the rail position of a host step.
func setupRailIndex(step string) int {
	for i, s := range setupSteps {
		for _, name := range s.steps {
			if name == step {
				return i
			}
		}
	}
	return len(setupSteps)
}

// setupStep draws the current step: its words, its checklist and progress,
// and its buttons.
func (s *desktop) setupStep(c *ui.Context, p palette, v hostSetup) {
	title := func(text string) {
		ui.Text(c, text).FontSize(22).FontWeight(650).TextColor(p.text)
	}
	body := func(text string) {
		ui.Text(c, text).FontSize(13.5).LineHeight(1.5).TextColor(p.sub)
	}
	busy := v.Busy || s.win.acting != ""
	switch v.Step {
	case "welcome":
		title(tr("在 Windows 上运行容器", "Run containers on Windows"))
		body(tr("Fabo 的引擎运行在 WSL 2 里。这台电脑还没有 WSL：Fabo 会从 Microsoft 下载并安装它，Windows 会请求一次管理员批准，之后可能需要重启。",
			"Fabo's engine runs in WSL 2, which this PC does not have yet. Fabo downloads it from Microsoft and installs it; Windows asks for an administrator's approval once, and may need to restart."))
		s.setupError(c, p, v)
		ui.Row(c).Gap(8).Children(func() {
			if button(c, p, tr("安装 WSL", "Install WSL"), "", primaryButton, busy) {
				s.setupAction("install-wsl", nil)
			}
			if openLink(c, p, "", tr("什么是 WSL？", "What is WSL?"), "https://learn.microsoft.com/windows/wsl/about") {
				c.OpenURL("https://learn.microsoft.com/windows/wsl/about")
			}
		})
	case "restart":
		title(tr("重启 Windows", "Restart Windows"))
		body(tr("WSL 已安装，重启后生效。Windows 重新登录后 Fabo 会自动打开并继续设置。",
			"WSL is installed and takes effect after a restart. Fabo opens by itself when you sign in again, and carries on."))
		ui.Row(c).Gap(8).Children(func() {
			if button(c, p, tr("立即重启", "Restart now"), "", primaryButton, busy) {
				s.setupAction("restart", nil)
			}
			if button(c, p, tr("稍后重启", "Restart later"), "", plainButton, false) && s.window != nil {
				s.window.Hide()
			}
		})
	case "choose":
		title(tr("选择引擎所在的 Linux", "Choose a Linux for the engine"))
		body(tr("Fabo Linux 是专为引擎准备的小型发行版，随 Fabo 安装，无需下载。选 Ubuntu 则把引擎装进一个完整的 Ubuntu，便于在其中做别的事。",
			"Fabo Linux is a small distro made for the engine and comes with Fabo: nothing to download. Ubuntu puts the engine in a full Ubuntu, for doing other things in it too."))
		ubuntu := tr("Ubuntu（下载约 420 MB）", "Ubuntu (about a 420 MB download)")
		if v.ExistingUbuntu != "" {
			ubuntu = fmt.Sprintf(tr("这台电脑上的 %s", "%s, already on this PC"), v.ExistingUbuntu)
		}
		ui.Segmented(c.Key("setup-linux"), &s.win.linux, tr("Fabo Linux（推荐）", "Fabo Linux (recommended)"), ubuntu).
			Label(tr("Linux", "Linux")).Height(30)
		makeDefault := v.MakeDefault
		ui.Row(c).Gap(10).Children(func() {
			if ui.Switch(c.Key("setup-default"), &makeDefault).Label(tr("让 docker 默认使用 Fabo", "Make Fabo the default for docker")).Changed() {
				s.setupAction("default", makeDefault)
			}
			ui.Text(c, tr("让 docker 默认使用 Fabo", "Make Fabo the default for docker")).FontSize(13).TextColor(p.text)
		})
		if v.OtherDocker {
			body(tr("这台电脑上还有另一个 Docker。不设为默认时，用 docker --context fabo 使用 Fabo。",
				"Another Docker is on this PC. Without making Fabo the default, use docker --context fabo for Fabo."))
		}
		s.setupError(c, p, v)
		ui.Row(c).Children(func() {
			if button(c, p, tr("继续", "Continue"), "", primaryButton, busy) {
				kind := "fabo"
				if s.win.linux == 1 {
					kind = "ubuntu"
				}
				s.setupAction("choose", kind)
			}
		})
	case "wsl", "linux", "engine", "tools":
		switch v.Step {
		case "wsl":
			title(tr("安装 WSL", "Installing WSL"))
		case "linux":
			title(tr("准备 Linux", "Setting up Linux"))
		case "engine":
			title(tr("启动引擎", "Starting the engine"))
		default:
			title(tr("安装 Docker 工具", "Installing the Docker tools"))
		}
		s.setupProgress(c, p, v)
		s.setupError(c, p, v)
		ui.Row(c).Gap(8).Children(func() {
			if v.Busy {
				if button(c, p, tr("取消", "Cancel"), "", plainButton, false) {
					s.setupAction("cancel", nil)
				}
			} else if v.Error != "" {
				if button(c, p, tr("重试", "Try again"), "", primaryButton, s.win.acting != "") {
					s.setupAction("retry", nil)
				}
			}
		})
	case "ready":
		title(tr("Fabo 已就绪", "Fabo is ready"))
		body(tr("在任意终端里试试：", "Try it in any terminal:"))
		ui.Text(c, "docker run --rm alpine echo hello").Font(mono).FontSize(13).TextColor(p.text).Selectable().
			Padding(10, 12).Radius(8).Background(p.soft).AlignSelf(ui.Start)
		autostart := v.Autostart
		ui.Row(c).Gap(10).Children(func() {
			if ui.Switch(c.Key("setup-autostart"), &autostart).Label(tr("登录时启动 Fabo", "Start Fabo when I sign in")).Changed() {
				s.setupAction("autostart", autostart)
			}
			ui.Text(c, tr("登录时启动 Fabo", "Start Fabo when I sign in")).FontSize(13).TextColor(p.text)
		})
		s.setupError(c, p, v)
		ui.Row(c).Children(func() {
			if button(c, p, tr("打开 Fabo", "Open Fabo"), "", primaryButton, busy) {
				s.setupAction("finish", nil)
			}
		})
	default:
		ui.Spinner(c)
	}
}

// setupProgress draws a running step's bar, checklist and recent log.
func (s *desktop) setupProgress(c *ui.Context, p palette, v hostSetup) {
	if pr := v.Progress; pr != nil {
		ui.Column(c.Key("setup-progress")).Gap(6).Children(func() {
			fraction := -1.0
			if pr.Pct != nil {
				fraction = float64(*pr.Pct) / 100
			}
			ui.Progress(c, fraction).FillWidth()
			line := pr.Label
			if pr.Done != nil && pr.Total != nil && *pr.Total > 0 {
				// MB of 10^6, as the host's checklist counts them.
				line += fmt.Sprintf(" · %d / %d MB", (*pr.Done+500_000)/1_000_000, (*pr.Total+500_000)/1_000_000)
				if pr.ETA != nil && *pr.ETA > 0 {
					line += fmt.Sprintf(tr(" · 剩余 %d 秒", " · %d s left"), *pr.ETA)
				}
			}
			ui.Text(c, line).FontSize(12).TextColor(p.sub)
		})
	}
	ui.Column(c.Key("setup-tasks")).Gap(8).Children(func() {
		for i, t := range v.Tasks {
			ui.Row(c.Key(fmt.Sprintf("task-%d", i))).Gap(10).Children(func() {
				switch t.State {
				case "done":
					ui.Icon(c, icons["check"]).Size(14, 14).TextColor(p.ok)
				case "active":
					ui.Spinner(c).Size(14, 14)
				case "error":
					ui.Box(c).Size(14, 14).Radius(7).Background(p.err)
				default:
					ui.Box(c).Size(14, 14).Radius(7).Border(1.5, p.muted.Alpha(0.5))
				}
				ink := p.text
				if t.State == "todo" {
					ink = p.muted
				}
				ui.Text(c, t.Label).FontSize(13).TextColor(ink)
				if t.Detail != "" {
					ui.Text(c, t.Detail).FontSize(12).TextColor(p.muted).FontFeatures("tnum")
				}
			})
		}
	})
	if len(v.Log) > 0 {
		ui.Column(c.Key("setup-log")).Gap(2).Padding(10, 12).Radius(8).Background(p.soft).Children(func() {
			for i, line := range v.Log[max(0, len(v.Log)-6):] {
				ui.Text(c.Key(fmt.Sprintf("log-%d", i)), line).Font(mono).FontSize(11.5).TextColor(p.sub).SingleLine()
			}
		})
	}
}

// setupError shows why the step stopped.
func (s *desktop) setupError(c *ui.Context, p palette, v hostSetup) {
	if v.Error == "" {
		return
	}
	ui.Text(c, v.Error).FontSize(12.5).TextColor(p.err).Selectable().Padding(10, 12).Radius(8).Background(p.err.Alpha(0.08))
}
