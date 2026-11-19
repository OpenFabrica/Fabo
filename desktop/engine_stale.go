package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/egoist/mygo/ui"
)

// staleEngine returns the Fabo release of the engine this window talks to
// (FaboVersion, not the Docker Engine version) when it
// differs from the window's own, as when an update or reinstall replaced the
// app while the old engine kept running: replacing the app does not restart
// it. A development window (0.0.0-dev) compares nothing, nor does one that
// borrows the installed app's engine (FABO_SHARED_ENGINE): that engine
// rightly runs the installed version, not the window's.
func (s *desktop) staleEngine() (string, bool) {
	engine := s.info.faboVersion()
	if release == "0.0.0-dev" || os.Getenv("FABO_SHARED_ENGINE") != "" || !s.connected || s.info.Name != "fabo" || engine == "" {
		return "", false
	}
	return engine, engine != release
}

// engineStrip asks to restart an engine of another version, as Kiri does
// after an update. It reports whether it showed, so the import strip waits.
func (s *desktop) engineStrip(c *ui.Context, p palette) bool {
	engine, stale := s.staleEngine()
	if !stale || s.staleLater || s.launcher == nil || s.page == terminalPage {
		return false
	}
	stripBar(c, p, "engine-stale", "info", p.warn, p.warn.Alpha(0.08),
		fmt.Sprintf(tr("引擎仍在运行 %s，", "The engine still runs %s,"), engine),
		fmt.Sprintf(tr("不是 %s %s。重启后，运行中的容器会停止，配了重启策略的会自动拉起。", "not %s %s. Restarting it stops running containers; those with a restart policy start again."), s.appName(), release),
		func() {
			if s.working["engine-restart"] {
				ui.Text(c, tr("重启中…", "Restarting…")).FontSize(12).TextColor(p.muted).SingleLine().Shrink(0)
				return
			}
			if stripButton(c.Key("engine-restart"), p, tr("重启引擎", "Restart engine")) {
				s.restartEngine()
			}
			if iconButton(c.Key("engine-later"), p, "x", tr("稍后", "Later"), false).Clicked() {
				s.staleLater = true
			}
		})
	return true
}

// restartEngine restarts the engine on the installed version through the
// launcher (`fabo engine restart`), which stops the workloads gracefully and
// waits for the new engine to answer.
func (s *desktop) restartEngine() {
	if s.launcher == nil || s.working["engine-restart"] {
		return
	}
	s.working["engine-restart"] = true
	s.run(3*time.Minute, func(ctx context.Context) func() {
		_, err := s.launcher(ctx, "engine", "restart")
		return func() {
			delete(s.working, "engine-restart")
			s.settle(err, tr("引擎已重启", "Engine restarted"))
		}
	})
	if s.engine.http == nil {
		delete(s.working, "engine-restart")
	}
}
