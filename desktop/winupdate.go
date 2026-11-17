package main

import (
	"context"
	"fmt"
	"time"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/ui"
)

// updateStrip offers a newer Fabo on Windows, which fabo-win checks for:
// download it, then restart into it. It reports whether it showed.
func (s *desktop) updateStrip(c *ui.Context, p palette) bool {
	u := s.win.update
	if s.host == nil || s.win.updateLater || s.page == terminalPage {
		return false
	}
	switch u.State {
	case "available":
		stripBar(c, p, "update", "info", p.accent, p.accent.Alpha(0.08),
			fmt.Sprintf(tr("Fabo %s 可用，", "Fabo %s is available,"), u.Latest),
			fmt.Sprintf(tr("当前是 %s。", "this is %s."), u.Current),
			func() {
				if stripButton(c.Key("update-download"), p, tr("下载", "Download")) {
					s.updateAction("download")
				}
				if iconButton(c.Key("update-later"), p, "x", tr("稍后", "Later"), false).Clicked() {
					s.win.updateLater = true
				}
			})
	case "downloading":
		detail := ""
		if u.Total > 0 {
			detail = fmt.Sprintf("%d%%", u.Done*100/u.Total)
		}
		stripBar(c, p, "update", "info", p.accent, p.accent.Alpha(0.08),
			fmt.Sprintf(tr("正在下载 Fabo %s ", "Downloading Fabo %s "), u.Latest), detail,
			func() {
				if stripButton(c.Key("update-cancel"), p, tr("取消", "Cancel")) {
					s.updateAction("cancel")
				}
			})
	case "ready":
		stripBar(c, p, "update", "info", p.ok, p.ok.Alpha(0.08),
			fmt.Sprintf(tr("Fabo %s 已下载，", "Fabo %s is downloaded,"), u.Latest),
			tr("重启 Fabo 即可完成更新；退出时也会安装。", "restart Fabo to finish updating; quitting installs it too."),
			func() {
				if stripButton(c.Key("update-install"), p, tr("立即重启", "Restart now")) {
					s.installUpdate()
				}
				if iconButton(c.Key("update-later"), p, "x", tr("稍后", "Later"), false).Clicked() {
					s.win.updateLater = true
				}
			})
	default:
		return false
	}
	return true
}

// updateAction sends one update action to fabo-win.
func (s *desktop) updateAction(action string) {
	host := s.host
	s.run(30*time.Second, func(ctx context.Context) func() {
		err := host.updateAction(ctx, action)
		u, uerr := host.update(ctx)
		return func() {
			if err != nil {
				s.notices = append(s.notices, err.Error())
			}
			if uerr == nil {
				s.win.update = u
			}
		}
	})
}

// installUpdate hands the downloaded installer to fabo-win, which quits for
// it, and quits the window: the installer replaces both, then opens Fabo.
func (s *desktop) installUpdate() {
	host := s.host
	s.run(30*time.Second, func(ctx context.Context) func() {
		err := host.updateAction(ctx, "install")
		return func() {
			if err != nil {
				s.notices = append(s.notices, err.Error())
				return
			}
			if s.window != nil {
				mygo.App.Quit()
			}
		}
	})
}
