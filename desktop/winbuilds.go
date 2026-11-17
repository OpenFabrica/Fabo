package main

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/egoist/mygo/ui"
)

// Which builder plain `docker build` uses on Windows, Docker's or Fabo's:
// fabo-win keeps the choice (engine/internal/buildwith) and the window
// shows and changes it in Settings → Builds, and asks once when another
// engine is on the PC. The Mac has `fabo build-with` but no control here
// yet, so this shows only with fabo-win.

// hostBuildWith is fabo-win's GET /buildwith.
type hostBuildWith struct {
	With    string `json:"with"`
	Ask     bool   `json:"ask"`
	Engine  string `json:"engine"`
	Taken   bool   `json:"taken"`
	Ready   bool   `json:"ready"`
	Builder string `json:"builder"`
	Error   string `json:"error"`
}

// buildWith reads which builder docker build uses.
func (h *hostClient) buildWith(ctx context.Context) (hostBuildWith, error) {
	var b hostBuildWith
	return b, h.call(ctx, http.MethodGet, "/buildwith", nil, &b)
}

// chooseBuildWith switches docker build to "docker" or "fabo".
func (h *hostClient) chooseBuildWith(ctx context.Context, with string) error {
	return h.call(ctx, http.MethodPost, "/buildwith", map[string]string{"value": with}, nil)
}

// buildWithIndex is the segmented control's index for a choice.
func buildWithIndex(with string) int {
	if with == "fabo" {
		return 1
	}
	return 0
}

// chooseBuildWith sends the window's choice and reads the result back. The
// control shows the choice at once; a failure puts the old one back.
func (s *desktop) chooseBuildWith(with string) {
	if s.host == nil || s.win.buildActing {
		return
	}
	s.win.buildActing, s.win.build.With, s.win.build.Ask = true, with, false
	host := s.host
	s.run(30*time.Second, func(ctx context.Context) func() {
		err := host.chooseBuildWith(ctx, with)
		b, rerr := host.buildWith(ctx)
		return func() {
			s.win.buildActing = false
			if err != nil {
				s.notices = append(s.notices, tr("无法更改 docker build 使用的构建器：", "Couldn't change which builder docker build uses: ")+err.Error())
			}
			if rerr == nil {
				s.win.build, s.win.buildKnown = b, true
			}
		}
	})
}

// buildWithRows is the top of Settings → Builds on Windows: Docker's
// builder or Fabo's for plain `docker build`.
func (s *desktop) buildWithRows(c *ui.Context, p palette) {
	b := s.win.build
	if !s.win.buildKnown {
		return
	}
	if b.Error != "" {
		remark(c, p, b.Error)
		return
	}
	fabo := b.With == "fabo"
	hint := tr("docker build 跟随 docker 当前使用的引擎；docker 使用 Fabo 时在 Fabo 上构建。", "docker build builds wherever docker points. When docker uses Fabo, builds run on Fabo.")
	switch {
	case fabo && b.Engine != "":
		hint = fmt.Sprintf(tr("docker build 和 docker compose build 都在 Fabo 上运行，镜像在 Fabo 里，%s 看不到。", "docker build and docker compose build run on Fabo, and the images are in Fabo, where %s doesn't see them."), b.Engine)
	case fabo:
		hint = tr("docker build 和 docker compose build 都在 Fabo 上运行，镜像在 Fabo 里。", "docker build and docker compose build run on Fabo, and the images are in Fabo.")
	case b.Engine != "":
		hint = fmt.Sprintf(tr("docker build 跟随 docker 当前使用的引擎，%s 的构建留在那里；docker 使用 Fabo 时在 Fabo 上构建。", "docker build builds wherever docker points, so %s's builds stay there. When docker uses Fabo, builds run on Fabo."), b.Engine)
	}
	pref(c, p, tr("构建方式", "Build with"), hint, func() {
		selected := buildWithIndex(b.With)
		if choice(c.Key("build-with"), p, tr("构建方式", "Build with"), &selected, "Docker", "Fabo") && !s.win.buildActing {
			s.chooseBuildWith([]string{"docker", "fabo"}[selected])
		}
	})
	switch {
	case fabo && !b.Ready:
		remark(c, p, fmt.Sprintf(tr("Fabo 的构建器 %s 还没建好，建好后构建会交给它。", "Fabo's builder %s isn't set up yet; builds go to it once it is."), b.Builder))
	case fabo && !b.Taken:
		remark(c, p, tr("之后又选了别的构建器（docker buildx use）。再选一次 Fabo 才会回到 Fabo 构建。", "Another builder has been picked since (docker buildx use). Choose Fabo again to build on Fabo."))
	}
	remark(c, p, tr("切换立即生效，已打开的终端也一样。切回 Docker 会把 docker 的构建设置恢复成 Fabo 改动之前的样子。", "A switch applies at once, in terminals already open too. Switching back to Docker puts docker's build settings back as they were before Fabo changed them."))
}

// buildAsk asks once, with another engine on the PC, which should build
// images. Keeping Docker is the default: Escape and the backdrop choose it.
func (s *desktop) buildAsk(c *ui.Context, p palette) {
	b := s.win.build
	if s.host == nil || !s.win.buildKnown || !b.Ask || s.win.buildAsked || s.pending != nil || s.form.kind != "" {
		return
	}
	open := true
	picked := ""
	ui.DialogBase(c.Key("build-ask"), &open, func(backdrop, dialog ui.Element) {
		backdrop.Background(ui.RGBA(0, 0, 0, 0.45))
		dialog.Width(440).Padding(16).Gap(12).Radius(8).Background(p.panel).Border(1, p.line).
			Shadow(0, 24, 60, -10, ui.RGBA(0, 0, 0, 0.5))
		ui.Text(c, fmt.Sprintf(tr("这台电脑上已有 %s。用哪个来构建镜像？", "You already have %s. Which should build your images?"), b.Engine)).
			FontSize(13.5).FontWeight(600).TextColor(p.text)
		ui.Text(c, fmt.Sprintf(tr("继续用 Docker 构建：docker build 照旧，镜像留在 %s。", "Keep building with Docker: docker build works as it does now, and the images stay in %s."), b.Engine)).
			FontSize(12).LineHeight(1.5).TextColor(p.muted)
		ui.Text(c, fmt.Sprintf(tr("用 Fabo 构建：docker build 和 docker compose build 在 Fabo 上运行，镜像在 Fabo 里，%s 看不到。", "Build with Fabo: docker build and docker compose build run on Fabo, and the images are in Fabo, where %s doesn't see them."), b.Engine)).
			FontSize(12).LineHeight(1.5).TextColor(p.muted)
		ui.Text(c, tr("随时可以在 设置 → 构建 里切换。", "You can switch any time in Settings → Builds.")).FontSize(11.5).TextColor(p.muted)
		ui.Row(c).FillWidth().Gap(8).Justify(ui.End).Padding(4, 0, 0).Children(func() {
			if button(c.Key("build-ask-fabo"), p, tr("用 Fabo 构建", "Build with Fabo"), "", plainButton, false) {
				picked = "fabo"
			}
			if button(c.Key("build-ask-docker"), p, tr("继续用 Docker 构建", "Keep building with Docker"), "", primaryButton, false) {
				picked = "docker"
			}
		})
	})
	if picked == "" && !open {
		picked = "docker"
	}
	if picked != "" {
		s.win.buildAsked = true
		s.chooseBuildWith(picked)
	}
}
