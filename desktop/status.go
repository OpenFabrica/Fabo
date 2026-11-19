package main

import (
	"fmt"

	"github.com/egoist/mygo/ui"
)

// statusBar summarizes the window-wide state from the existing refreshes.
func (s *desktop) statusBar(c *ui.Context, p palette) {
	ui.Box(c).FillWidth().Height(1).Shrink(0).Background(p.line)
	ui.Row(c.Key("status-bar")).FillWidth().Height(26).Shrink(0).Padding(0, 12).Gap(12).AlignItems(ui.Center).Children(func() {
		ui.Row(c.Key("status-version")).MinWidth(0).Gap(6).AlignItems(ui.Center).Children(func() {
			ui.Image(c, markIcon).Size(12, 12).Shrink(0)
			statusText(c, p.muted, s.appName()+" "+release).MinWidth(0)
		})
		ui.Box(c).Grow(1).MinWidth(0)
		kind, text := s.engineWords()
		ui.Row(c.Key("status-engine")).Shrink(0).Gap(7).AlignItems(ui.Center).Children(func() {
			dot(c, p, kind)
			statusText(c, p.muted, s.vmName()+" · "+text)
			// The engine's Fabo release, not the Docker Engine version.
			if v := s.info.faboVersion(); s.connected && v != "" && v != "0.0.0" {
				ink := p.muted
				if _, stale := s.staleEngine(); stale {
					ink = p.warn
				}
				ui.Row(c.Key("status-engine-version")).Tooltip(tr("版本", "Version")).Children(func() {
					statusText(c, ink, "v"+v).MaxWidth(140)
				})
			}
		})
		if kind == "running" {
			running := 0
			for _, container := range s.containers {
				if container.State == "running" {
					running++
				}
			}
			count := fmt.Sprintf(tr("%d/%d 运行中", "%d/%d running"), running, len(s.containers))
			if statusButton(c.Key("status-containers"), p, p.muted, count).Clicked() {
				s.navigate(containersPage)
			}
			// Usage samples are page-specific. The engine reports capacity on
			// every page, so the status bar can show it consistently.
			if _, total := s.memoryUse(); total > 0 {
				ui.Row(c.Key("status-memory")).Shrink(0).Tooltip(tr("内存", "Memory")).Children(func() {
					statusText(c, p.muted, tr("内存", "Memory")+" "+displaySize(total))
				})
			}
			if s.clusterRunning() {
				if statusButton(c.Key("status-kubernetes"), p, p.muted, "Kubernetes").Clicked() {
					s.navigate(kubernetesPage)
				}
			}
		}
		if s.imp.ctx.Ours != "" && s.imp.ctx.Current != "" {
			if s.dockerElsewhere() && s.canImport() {
				title := fmt.Sprintf(tr("切换到 %s", "Switch to %s"), s.appName())
				if statusButton(c.Key("status-docker"), p, p.warn, "docker → "+contextLabel(s.imp.ctx.Current)).Tooltip(title).MaxWidth(200).Clicked() {
					s.openSwitch()
				}
			} else {
				statusText(c, p.muted, "docker → "+s.appName()).Shrink(0)
			}
		}
		if statusButton(c.Key("status-feedback"), p, p.muted, tr("反馈", "Feedback")).Tooltip(tr("GitHub 反馈", "Feedback on GitHub")).Clicked() {
			c.OpenURL(issuesURL)
		}
	})
}

func statusText(c *ui.Context, ink ui.Color, text string) ui.Element {
	return ui.Text(c, text).FontSize(11).FontFeatures("tnum").TextColor(ink).SingleLine()
}

func statusButton(c *ui.Context, p palette, ink ui.Color, text string) ui.Element {
	b := clickable(c).Label(text).Height(22).Padding(0, 5).Radius(5).AlignItems(ui.Center)
	if b.Hovered() {
		b.Background(p.raised)
	}
	b.Children(func() { statusText(c, ink, text).MinWidth(0) })
	return b
}
