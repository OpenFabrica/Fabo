package main

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/egoist/mygo/ui"
)

// onMain applies a change from a worker. Without a window, as in tests, the
// worker is already on the caller's thread.
func (s *desktop) onMain(fn func()) {
	if s.window == nil {
		fn()
		return
	}
	s.window.Update(fn)
}

// imageOptional are the columns left out beside a detail panel: ID, the
// number of containers and the creation time. A tight table also leaves out
// the security summary, which the panel's Security tab shows.
var (
	imageOptional = []int{1, 4, 5}
	imageTight    = []int{1, 3, 4, 5}
)

// imageColumns are shared by the header and the rows of the images table.
func imageColumns() []column {
	return []column{
		{tr("仓库 : 标签", "Repository : tag"), 0, 1.4, false}, {"ID", 112, 0, false}, {tr("大小", "Size"), 78, 0, true},
		{tr("安全", "Security"), 150, 0, false}, {tr("容器", "Containers"), 70, 0, false}, {tr("创建", "Created"), 78, 0, false},
	}
}

// scanWords summarises what is known about an image's scan for the table.
func (s *desktop) scanWords(p palette, img image) (string, ui.Color) {
	if pct, busy := s.scanning[img.ID]; busy {
		return fmt.Sprintf(tr("扫描中 %.0f%%", "Scanning %.0f%%"), pct*100), p.warn
	}
	result, known := s.scans[img.ID]
	switch {
	case !known:
		return "—", p.muted
	case result == nil:
		return tr("未扫描", "Not scanned"), p.muted
	}
	n := result.counts()
	var parts []string
	names := []string{tr("严重", "crit"), tr("高", "high"), tr("中", "med")}
	for i, name := range names {
		if n[i] > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n[i], name))
		}
	}
	switch {
	case len(result.Vulns) == 0:
		return tr("无漏洞", "Clean"), p.ok
	case len(parts) == 0:
		return fmt.Sprintf(tr("%d 低", "%d low"), len(result.Vulns)), p.muted
	case n[0]+n[1] > 0:
		return strings.Join(parts, " · "), p.err
	}
	return strings.Join(parts, " · "), p.warn
}

// startScan scans an image for known vulnerabilities, showing progress in
// its row until the result arrives.
func (s *desktop) startScan(img image) {
	s.scanning[img.ID] = 0
	s.run(15*time.Minute, func(ctx context.Context) func() {
		result, err := s.engine.scan(ctx, img.ref(), func(pct float64) {
			s.onMain(func() { s.scanning[img.ID] = pct })
		})
		return func() {
			delete(s.scanning, img.ID)
			if err != nil {
				s.notices = append(s.notices, err.Error())
				return
			}
			s.scans[img.ID] = result
		}
	})
	if s.engine.http == nil {
		delete(s.scanning, img.ID)
	}
}

// imagesView is the images page.
func (s *desktop) imagesView(c *ui.Context, p palette) {
	query := strings.ToLower(strings.TrimSpace(s.search))
	filter := s.filter[imagesPage]
	var shown, idle []image
	var total int64
	used := 0
	for _, img := range s.images {
		inUse := len(s.imageUsers(img)) > 0
		total += img.Size
		if inUse {
			used++
		} else {
			idle = append(idle, img)
		}
		if (filter == 0 || (filter == 1) == inUse) && strings.Contains(strings.ToLower(strings.Join(img.Tags, " ")+" "+img.ID), query) {
			shown = append(shown, img)
		}
	}
	toolbar(c, func() {
		labels := []string{tr("全部", "All"), tr("使用中", "In use"), tr("未使用", "Unused")}
		at := filter
		if filters(c.Key("filter"), p, tr("按使用情况筛选", "Filter by use"), &at, labels, []int{len(s.images), used, len(idle)}) {
			s.filter[imagesPage] = at
			s.list(imagesPage).ScrollTo(0, ui.Start)
		}
		ui.Box(c).Grow(1)
		// The filters take about 250, Remove unused about 130, Import about
		// 90 and Pull image about 110; a narrower table drops the first
		// button, then keeps only the icons of the others.
		wide := s.listWidth()
		if len(idle) > 0 && wide >= 640 && button(c.Key("prune"), p, fmt.Sprintf(tr("清理未使用 %d", "Remove unused %d"), len(idle)), "", ghostButton, false) {
			s.pending = &confirmation{
				title: fmt.Sprintf(tr("清理 %d 个未使用的镜像？", "Remove %d unused images?"), len(idle)), ok: tr("清理", "Remove"),
				run: func() {
					// dangling=false widens the prune from untagged images to every unused one.
					path := "/images/prune?filters=" + url.QueryEscape(`{"dangling":["false"]}`)
					s.do("prune", engineAction{http.MethodPost, path, nil}, tr("已清理未使用的镜像", "Unused images removed"))
				},
			}
		}
		loading := s.working["load"]
		load := tr("导入", "Import")
		if loading {
			load = tr("导入中…", "Importing…")
		}
		if wide >= 620 {
			if button(c.Key("load"), p, load, "import", plainButton, loading) {
				s.loadImage()
			}
		} else if iconButton(c.Key("load"), p, "import", tr("导入镜像归档", "Import an image archive"), loading).Clicked() {
			s.loadImage()
		}
		pulling := s.working["pull"]
		name := tr("拉取镜像", "Pull image")
		if pulling {
			name = tr("拉取中…", "Pulling…")
		}
		if wide < 460 {
			b := ui.ButtonBase(c.Key("pull")).Label(name).Tooltip(name).Size(32, 32).Shrink(0).Radius(8).Center().
				Cursor(ui.CursorPointer).Background(p.accent).Disabled(pulling)
			if b.Hovered() {
				b.Background(p.accent.Mix(p.text, 0.1))
			}
			b.Children(func() { ui.Icon(c, icons["download"]).Size(16, 16).TextColor(p.onAccent) })
			if b.Clicked() {
				s.form = form{kind: "pull"}
			}
		} else if button(c.Key("pull"), p, name, "download", primaryButton, pulling) {
			s.form = form{kind: "pull"}
		}
	})
	cols := imageColumns()
	// Size, Security, three buttons and the padding take about 380; below
	// 520 the name would have less than 140 left.
	narrow, tight := s.narrow(), s.listWidth() < 520
	// Beside a detail panel the row keeps three buttons; Pack moves to the
	// panel's head.
	rowPack := s.selected[imagesPage] == ""
	actionsWidth := float32(92)
	if rowPack {
		actionsWidth = 124
	}
	if !s.dockMax {
		optional := imageOptional
		if tight {
			optional = imageTight
		}
		tableHeader(c, p, narrowColumns(cols, narrow, optional...), actionsWidth, nil)
		if len(shown) == 0 {
			title := tr("没有匹配的镜像", "No matching images")
			if len(s.images) == 0 {
				title = tr("还没有镜像", "No images yet")
			}
			empty(c, p, "layers", title, "")
		} else {
			ui.List(c.Key("rows"), s.list(imagesPage), len(shown)).FillWidth().Grow(1).MinHeight(0).
				ItemKey(func(i int) any { return shown[i].ID }).
				Rows(func(r ui.ListRow) {
					rc, img := r.Context, shown[r.Index]
					users := len(s.imageUsers(img))
					s.tableRow(rc, p, img.ID, img.ref(), actionsWidth, nil, func() {
						cell(rc, 0, cols[0].grow).Children(func() { label(rc, img.ref(), p.text).FontWeight(500) })
						if !narrow {
							cell(rc, cols[1].width, 0).Children(func() { label(rc, shortID(img.ID), p.muted) })
						}
						cell(rc, cols[2].width, 0).Children(func() { number(rc, displaySize(img.Size), p.text) })
						if !tight {
							cell(rc, cols[3].width, 0).Children(func() {
								text, ink := s.scanWords(p, img)
								label(rc, text, ink)
							})
						}
						if narrow {
							return
						}
						cell(rc, cols[4].width, 0).Children(func() {
							if users > 0 {
								label(rc, strconv.Itoa(users), p.text)
							} else {
								label(rc, "—", p.muted)
							}
						})
						cell(rc, cols[5].width, 0).Children(func() { label(rc, since(s.now, img.Created), p.muted) })
					}, func() {
						if iconButton(rc.Key("run"), p, "play", tr("运行 ", "Run ")+img.ref(), len(img.Tags) == 0).Clicked() {
							s.form = form{kind: "run", image: img.ref(), named: true}
						}
						if rowPack && iconButton(rc.Key("save"), p, "archive", tr("打包 ", "Pack ")+img.ref(), s.working["save:"+img.ID]).Clicked() {
							s.saveImage(img)
						}
						_, scanning := s.scanning[img.ID]
						if iconButton(rc.Key("scan"), p, "shield", tr("扫描 ", "Scan ")+img.ref(), scanning).Clicked() {
							s.startScan(img)
						}
						if iconButton(rc.Key("delete"), p, "trash", tr("删除 ", "Delete ")+img.ref(), users > 0).Clicked() {
							s.pending = &confirmation{
								title: tr("删除镜像 ", "Delete image ") + img.ref() + tr("？", "?"), ok: tr("删除", "Delete"),
								run: func() {
									s.do(img.ID, engineAction{http.MethodDelete, "/images/" + url.PathEscape(img.ref()), nil}, tr("已删除 ", "Deleted ")+img.ref())
								},
							}
						}
					})
				})
		}
	}
	if img, ok := s.image(s.selected[imagesPage]); ok {
		s.imageDock(c, p, img)
	}
}

// imageDock shows the selected image: its layers, its scan and its config.
func (s *desktop) imageDock(c *ui.Context, p palette, img image) {
	key := imagesPage + "/" + img.ID
	if s.detail.key != key {
		s.detail = detail{key: key}
		s.loadDetail(key)
	}
	layersTab := tr("分层", "Layers")
	if n := len(s.detail.layers); n > 0 {
		layersTab += fmt.Sprintf(" %d", n)
	}
	result := s.scans[img.ID]
	pct, scanning := s.scanning[img.ID]
	s.dock(c, p, img.ref(), func() { label(c, displaySize(img.Size), p.muted).Shrink(0) },
		[]string{layersTab, tr("安全扫描", "Security"), tr("信息", "Info")},
		func() {
			if iconButton(c.Key("save"), p, "archive", tr("打包 ", "Pack ")+img.ref(), s.working["save:"+img.ID]).Clicked() {
				s.saveImage(img)
			}
			if s.tab[imagesPage] == 1 && result != nil && button(c.Key("rescan"), p, tr("重新扫描", "Rescan"), "", ghostButton, scanning) {
				s.startScan(img)
			}
		},
		func(tab int) {
			switch tab {
			case 0:
				s.layersTab(c, p, img)
			case 1:
				switch {
				case scanning:
					ui.Column(c).Fill().Grow(1).Center().Gap(12).Children(func() {
						ui.Text(c, fmt.Sprintf(tr("扫描中 %.0f%%", "Scanning %.0f%%"), pct*100)).FontSize(13).TextColor(p.text)
						ui.Box(c).Width(260).Children(func() { meter(c, p, pct, 4, p.accent) })
					})
				case result == nil:
					if empty(c, p, "shield", tr("尚未扫描", "Not scanned yet"), tr("开始扫描", "Scan now")) {
						s.startScan(img)
					}
				case len(result.Vulns) == 0:
					empty(c, p, "check", tr("未发现已知漏洞", "No known vulnerabilities"), "")
				default:
					s.findingsTab(c, p, result)
				}
			case 2:
				keyValues(c, p, s.detail.fields)
			}
		})
}

// layersTab lists the image's layers with each one's share of the size.
func (s *desktop) layersTab(c *ui.Context, p palette, img image) {
	layers := s.detail.layers
	if len(layers) == 0 {
		note(c, p, tr("没有分层信息", "No layer information"))
		return
	}
	ui.List(c.Key("layers"), s.list("layers"), len(layers)).FillWidth().Grow(1).MinHeight(0).Rows(func(r ui.ListRow) {
		rc, l := r.Context, layers[r.Index]
		ui.Row(rc).FillWidth().Height(26).PaddingX(14).Gap(10).AlignItems(ui.Center).BorderWidth(0, 0, 1, 0).BorderColor(p.line).Children(func() {
			cell(rc, 24, 0).Children(func() { number(rc, strconv.Itoa(r.Index+1), p.muted) })
			cell(rc, 0, 1).Children(func() { label(rc, strings.Join(strings.Fields(l.By), " "), p.text) })
			ui.Box(rc).Width(120).Shrink(0).Children(func() {
				meter(rc, p, float64(l.Size)/float64(max(1, img.Size)), 4, p.muted)
			})
			cell(rc, 72, 0).Children(func() { number(rc, displaySize(l.Size), p.text) })
		})
	})
}

// findingsTab lists a scan's findings, the worst first.
func (s *desktop) findingsTab(c *ui.Context, p palette, result *scanResult) {
	found := append([]finding(nil), result.Vulns...)
	sort.SliceStable(found, func(i, j int) bool { return severityRank(found[i].Severity) < severityRank(found[j].Severity) })
	ui.List(c.Key("findings"), s.list("findings"), len(found)).FillWidth().Grow(1).MinHeight(0).Rows(func(r ui.ListRow) {
		rc, f := r.Context, found[r.Index]
		ink := p.muted
		switch severityRank(f.Severity) {
		case 0, 1:
			ink = p.err
		case 2:
			ink = p.warn
		}
		ui.Row(rc).FillWidth().Height(26).PaddingX(14).Gap(10).AlignItems(ui.Center).BorderWidth(0, 0, 1, 0).BorderColor(p.line).Children(func() {
			cell(rc, 170, 0).Children(func() { label(rc, f.ID, p.text) })
			cell(rc, 84, 0).Children(func() { label(rc, f.Severity, ink).FontWeight(600) })
			cell(rc, 0, 1).Children(func() { label(rc, f.Package, p.text) })
			cell(rc, 130, 0).Children(func() { label(rc, f.Version, p.muted) })
			cell(rc, 130, 0).Children(func() {
				if len(f.Fixed) > 0 {
					label(rc, f.Fixed[0], p.ok)
				} else {
					label(rc, "—", p.muted)
				}
			})
		})
	})
}

// volumesView is the volumes page.
func (s *desktop) volumesView(c *ui.Context, p palette) {
	query := strings.ToLower(strings.TrimSpace(s.search))
	var shown []volume
	idle := 0
	for _, v := range s.volumes {
		if len(s.volumeUsers(v.Name)) == 0 {
			idle++
		}
		if strings.Contains(strings.ToLower(v.Name), query) {
			shown = append(shown, v)
		}
	}
	size := func(v volume) string {
		if s.disk == nil {
			return "—"
		}
		return displaySize(s.disk.volumes[v.Name])
	}
	toolbar(c, func() {
		ui.Text(c, fmt.Sprintf(tr("%d 个卷", "%d volumes"), len(s.volumes))).FontSize(12).FontFeatures("tnum").TextColor(p.muted)
		ui.Box(c).Grow(1)
		if idle > 0 && button(c.Key("prune"), p, fmt.Sprintf(tr("清理未使用 %d", "Remove unused %d"), idle), "", ghostButton, false) {
			s.pending = &confirmation{
				title: fmt.Sprintf(tr("清理 %d 个未使用的卷？", "Remove %d unused volumes?"), idle),
				body:  tr("卷里的数据无法恢复。", "Their data cannot be recovered."), ok: tr("清理", "Remove"),
				run: func() {
					// Without all=true only anonymous volumes are pruned.
					path := "/volumes/prune?filters=" + url.QueryEscape(`{"all":["true"]}`)
					s.do("prune", engineAction{http.MethodPost, path, nil}, tr("已清理未使用的卷", "Unused volumes removed"))
				},
			}
		}
		if button(c.Key("create"), p, tr("创建卷", "Create volume"), "plus", primaryButton, false) {
			s.form = form{kind: "volume"}
		}
	})
	cols := []column{{tr("名称", "Name"), 0, 1, false}, {tr("驱动", "Driver"), 80, 0, false}, {tr("大小", "Size"), 86, 0, true},
		{tr("使用者", "Used by"), 0, 1.2, false}, {tr("创建", "Created"), 86, 0, false}}
	const actionsWidth = 28
	if !s.dockMax {
		tableHeader(c, p, cols, actionsWidth, nil)
		if len(shown) == 0 {
			title := tr("没有匹配的卷", "No matching volumes")
			if len(s.volumes) == 0 {
				title = tr("还没有卷", "No volumes yet")
			}
			empty(c, p, "cylinder", title, "")
		} else {
			ui.List(c.Key("rows"), s.list(volumesPage), len(shown)).FillWidth().Grow(1).MinHeight(0).
				ItemKey(func(i int) any { return shown[i].Name }).
				Rows(func(r ui.ListRow) {
					rc, v := r.Context, shown[r.Index]
					users := s.volumeUsers(v.Name)
					names := make([]string, len(users))
					for i, x := range users {
						names[i] = x.Name
					}
					s.tableRow(rc, p, v.Name, v.Name, actionsWidth, nil, func() {
						cell(rc, 0, cols[0].grow).Children(func() { label(rc, v.Name, p.text).FontWeight(500) })
						cell(rc, cols[1].width, 0).Children(func() { label(rc, v.Driver, p.muted) })
						cell(rc, cols[2].width, 0).Children(func() { number(rc, size(v), p.text) })
						cell(rc, 0, cols[3].grow).Children(func() {
							if len(names) > 0 {
								label(rc, strings.Join(names, ", "), p.text)
							} else {
								label(rc, "—", p.muted)
							}
						})
						cell(rc, cols[4].width, 0).Children(func() { label(rc, since(s.now, v.Created), p.muted) })
					}, func() {
						if iconButton(rc.Key("delete"), p, "trash", tr("删除 ", "Delete ")+v.Name, len(users) > 0).Clicked() {
							s.pending = &confirmation{
								title: tr("删除卷 ", "Delete volume ") + v.Name + tr("？", "?"),
								body:  tr("卷里的数据无法恢复。", "Its data cannot be recovered."), ok: tr("删除", "Delete"),
								run: func() {
									s.do(v.Name, engineAction{http.MethodDelete, "/volumes/" + url.PathEscape(v.Name), nil}, tr("已删除 ", "Deleted ")+v.Name)
								},
							}
						}
					})
				})
		}
	}
	for _, v := range s.volumes {
		if v.Name != s.selected[volumesPage] {
			continue
		}
		users := s.volumeUsers(v.Name)
		s.dock(c, p, v.Name, nil, []string{tr("信息", "Info"), fmt.Sprintf(tr("使用者 %d", "Used by %d"), len(users))}, nil, func(tab int) {
			if tab == 1 {
				s.users(c, p, users)
				return
			}
			keyValues(c, p, compact([]field{{tr("名称", "Name"), v.Name}, {tr("驱动", "Driver"), v.Driver},
				{tr("挂载点", "Mountpoint"), v.Mountpoint}, {tr("大小", "Size"), strings.TrimPrefix(size(v), "—")}, {tr("创建", "Created"), since(s.now, v.Created)}}))
		})
	}
}

// networksView is the networks page.
func (s *desktop) networksView(c *ui.Context, p palette) {
	query := strings.ToLower(strings.TrimSpace(s.search))
	var shown []network
	for _, n := range s.networks {
		if strings.Contains(strings.ToLower(n.Name), query) {
			shown = append(shown, n)
		}
	}
	toolbar(c, func() {
		ui.Text(c, fmt.Sprintf(tr("%d 个网络", "%d networks"), len(s.networks))).FontSize(12).FontFeatures("tnum").TextColor(p.muted)
		ui.Box(c).Grow(1)
		if button(c.Key("create"), p, tr("创建网络", "Create network"), "plus", primaryButton, false) {
			s.form = form{kind: "network"}
		}
	})
	cols := []column{{tr("名称", "Name"), 0, 1, false}, {tr("驱动", "Driver"), 80, 0, false}, {tr("子网", "Subnet"), 150, 0, false},
		{tr("网关", "Gateway"), 120, 0, false}, {tr("容器", "Containers"), 70, 0, false}, {tr("创建", "Created"), 86, 0, false}}
	const actionsWidth = 28
	dash := func(rc *ui.Context, text string, ink ui.Color) {
		if text == "" {
			label(rc, "—", p.muted)
			return
		}
		label(rc, text, ink)
	}
	if !s.dockMax {
		tableHeader(c, p, cols, actionsWidth, nil)
		if len(shown) == 0 {
			empty(c, p, "network", tr("没有匹配的网络", "No matching networks"), "")
		} else {
			ui.List(c.Key("rows"), s.list(networksPage), len(shown)).FillWidth().Grow(1).MinHeight(0).
				ItemKey(func(i int) any { return shown[i].ID }).
				Rows(func(r ui.ListRow) {
					rc, n := r.Context, shown[r.Index]
					users := len(s.networkUsers(n.Name))
					s.tableRow(rc, p, n.ID, n.Name, actionsWidth, nil, func() {
						cell(rc, 0, cols[0].grow).Gap(8).Children(func() {
							label(rc, n.Name, p.text).FontWeight(500)
							if n.builtin() {
								ui.Text(rc, tr("内置", "built-in")).FontSize(11).TextColor(p.sub).Padding(1, 7).Radius(10).Background(p.soft).Shrink(0)
							}
						})
						cell(rc, cols[1].width, 0).Children(func() { label(rc, n.Driver, p.muted) })
						cell(rc, cols[2].width, 0).Children(func() { dash(rc, n.Subnet, p.text) })
						cell(rc, cols[3].width, 0).Children(func() { dash(rc, n.Gateway, p.muted) })
						cell(rc, cols[4].width, 0).Children(func() {
							if users > 0 {
								label(rc, strconv.Itoa(users), p.text)
							} else {
								label(rc, "—", p.muted)
							}
						})
						cell(rc, cols[5].width, 0).Children(func() { label(rc, since(s.now, n.Created), p.muted) })
					}, func() {
						if iconButton(rc.Key("delete"), p, "trash", tr("删除 ", "Delete ")+n.Name, n.builtin() || users > 0).Clicked() {
							s.pending = &confirmation{
								title: tr("删除网络 ", "Delete network ") + n.Name + tr("？", "?"), ok: tr("删除", "Delete"),
								run: func() {
									s.do(n.ID, engineAction{http.MethodDelete, "/networks/" + url.PathEscape(n.ID), nil}, tr("已删除 ", "Deleted ")+n.Name)
								},
							}
						}
					})
				})
		}
	}
	for _, n := range s.networks {
		if n.ID != s.selected[networksPage] {
			continue
		}
		users := s.networkUsers(n.Name)
		s.dock(c, p, n.Name, func() { label(c, n.Driver, p.muted).Shrink(0) },
			[]string{tr("信息", "Info"), fmt.Sprintf(tr("容器 %d", "Containers %d"), len(users))}, nil, func(tab int) {
				if tab == 1 {
					s.users(c, p, users)
					return
				}
				keyValues(c, p, compact([]field{{"ID", n.ID}, {tr("驱动", "Driver"), n.Driver}, {tr("子网", "Subnet"), n.Subnet},
					{tr("网关", "Gateway"), n.Gateway}, {tr("创建", "Created"), strings.TrimPrefix(since(s.now, n.Created), "—")}}))
			})
	}
}

// buildsView shows the builder. The engine keeps no record of past builds,
// so the page is the builder's state and its cache, the same rows as
// Settings → Builds.
func (s *desktop) buildsView(c *ui.Context, p palette) {
	ui.Scroll(c.Key("builds")).Fill().Grow(1).MinHeight(0).Children(func() {
		ui.Column(c).FillWidth().MaxWidth(760).Padding(18, 28, 24).Children(func() {
			s.builderRows(c, p)
			ui.Text(c, tr("引擎目前不记录构建历史。", "The engine does not keep a build history yet.")).FontSize(12).TextColor(p.muted).Padding(16, 0, 0)
		})
	})
}

// tile is one count of the overview: a card that opens its page. The
// page's icon sits in an accent chip at its corner.
func (s *desktop) tile(c *ui.Context, p palette, page, title, big, small string, ink ui.Color) {
	b := clickable(c.Key("tile-"+page)).Label(title).Grow(1).Width(0).MinWidth(0).Padding(16, 18).
		AlignItems(ui.Stretch).Justify(ui.Start).Radius(12).Background(p.panel).Border(1, p.line)
	if b.Hovered() {
		b.Border(1, p.raised.Mix(p.text, 0.08)).Shadow(0, 2, 6, 0, ui.RGBA(16, 24, 40, 0.05))
	}
	icon := "grid"
	for _, n := range navItems {
		if n.page == page {
			icon = n.icon
		}
	}
	// A button lays its children in a row; the card stacks a label line
	// over the figure.
	b.Children(func() {
		ui.Column(c).Grow(1).Width(0).MinWidth(0).Gap(12).Children(func() {
			ui.Row(c).FillWidth().AlignItems(ui.Center).Children(func() {
				ui.Text(c, title).FontSize(13).TextColor(p.sub).Grow(1)
				ui.Box(c).Size(30, 30).Radius(8).Background(p.accent.Alpha(0.1)).Center().Children(func() {
					ui.Icon(c, icons[icon]).Size(16, 16).TextColor(p.accent)
				})
			})
			ui.Row(c).FillWidth().Gap(8).AlignItems(ui.End).Children(func() {
				ui.Text(c, big).FontSize(28).FontWeight(600).LetterSpacing(-0.6).FontFeatures("tnum").TextColor(p.text).Shrink(0)
				ui.Text(c, small).FontSize(12.5).FontFeatures("tnum").TextColor(ink).SingleLine().MinWidth(0).Padding(0, 0, 5)
			})
		})
	})
	if b.Clicked() {
		s.navigate(page)
	}
}

// panel is one titled card of the overview grid.
func panel(c *ui.Context, p palette, grow float32, head, body func()) {
	ui.Column(c).Grow(grow).Width(0).MinWidth(0).Padding(16, 18, 18).Gap(12).
		Radius(12).Background(p.panel).Border(1, p.line).Children(func() {
		ui.Row(c).FillWidth().Height(28).Gap(10).AlignItems(ui.Center).Children(head)
		body()
	})
}

// overview is the engine at a glance: counts, load, disk, the busiest
// containers and what happened lately.
func (s *desktop) overview(c *ui.Context, p palette) {
	running, failed := 0, 0
	for _, x := range s.containers {
		switch x.bucket() {
		case "running":
			running++
		case "failed":
			failed++
		}
	}
	var imageSize int64
	for _, img := range s.images {
		imageSize += img.Size
	}
	head := func(title string) { ui.Text(c, title).FontSize(14).FontWeight(600).TextColor(p.text) }
	figure := func(text string) { ui.Text(c, text).FontSize(12).FontFeatures("tnum").TextColor(p.muted) }
	ui.Scroll(c.Key("overview")).Fill().Grow(1).MinHeight(0).Children(func() {
		ui.Column(c.Key("cards")).FillWidth().Padding(0, pageInset, pageInset).Gap(14).Children(func() {
			ui.Row(c.Key("tiles")).FillWidth().Gap(14).AlignItems(ui.Stretch).Children(func() {
				small, ink := tr("运行中", "running"), p.muted
				if failed > 0 {
					small, ink = fmt.Sprintf(tr("%d 个异常", "%d failed"), failed), p.err
				}
				s.tile(c, p, containersPage, tr("容器", "Containers"), fmt.Sprintf("%d / %d", running, len(s.containers)), small, ink)
				s.tile(c, p, imagesPage, tr("镜像", "Images"), strconv.Itoa(len(s.images)), displaySize(imageSize), p.muted)
				s.tile(c, p, volumesPage, tr("卷", "Volumes"), strconv.Itoa(len(s.volumes)), tr("本地", "local"), p.muted)
				s.tile(c, p, networksPage, tr("网络", "Networks"), strconv.Itoa(len(s.networks)), "bridge", p.muted)
			})
			ui.Row(c.Key("load")).FillWidth().Gap(14).AlignItems(ui.Stretch).Children(func() {
				panel(c, p, 1.5, func() {
					head("CPU")
					figure(fmt.Sprintf("%.0f%%", s.cpuNow()))
					figure(fmt.Sprintf(tr("%d 核", "%d cores"), s.cores()))
				}, func() {
					s.cpuChart(c, p)
					used, total := s.memoryUse()
					ui.Row(c).FillWidth().Gap(10).AlignItems(ui.Center).Children(func() {
						head(tr("内存", "Memory"))
						text := displaySize(used)
						if total > 0 {
							text += " / " + displaySize(total)
						}
						figure(text)
					})
					meter(c, p, float64(used)/float64(max(1, total)), 6, p.accent)
				})
				panel(c, p, 1, func() {
					head(tr("磁盘", "Disk"))
					ui.Box(c).Grow(1)
					if button(c.Key("cleanup"), p, tr("清理", "Clean up"), "", plainButton, false) {
						s.pending = &confirmation{
							title: tr("清理未使用的数据？", "Remove unused data?"),
							body:  tr("删除已停止的容器、未使用的镜像和构建缓存。", "Removes stopped containers, unused images and the build cache."),
							ok:    tr("清理", "Remove"), run: s.cleanup,
						}
					}
				}, func() { s.diskBody(c, p) })
			})
			ui.Row(c.Key("lists")).FillWidth().Gap(14).AlignItems(ui.Stretch).Children(func() {
				panel(c, p, 1.5, func() { head(tr("占用最高", "Busiest")) }, func() { s.busiest(c, p) })
				panel(c, p, 1, func() { head(tr("最近事件", "Recent events")) }, func() { s.recent(c, p) })
			})
		})
	})
}

// cleanup removes stopped containers, then unused images, then the build cache.
func (s *desktop) cleanup() {
	s.disk = nil
	s.run(5*time.Minute, func(ctx context.Context) func() {
		var err error
		for _, path := range []string{"/containers/prune", "/images/prune?filters=" + url.QueryEscape(`{"dangling":["false"]}`), "/build/prune?all=1"} {
			if e := s.engine.request(ctx, http.MethodPost, path, nil, nil); e != nil && err == nil {
				err = e
			}
		}
		return func() { s.settle(err, tr("已清理未使用的数据", "Unused data removed")) }
	})
}

// cpuChart plots the engine's CPU load over the last minutes. The scale
// follows the peak, with a floor so that a quiet engine draws a low line.
func (s *desktop) cpuChart(c *ui.Context, p palette) {
	values := s.cpuHist
	ui.Box(c.Key("cpu-chart")).FillWidth().Height(110).Draw(func(pt *ui.Painter, r ui.Rect) {
		for i := 1; i < 4; i++ {
			y := r.Y + r.H*float32(i)/4
			pt.Line(r.X, y, r.X+r.W, y, 1, p.line)
		}
		if len(values) < 2 {
			return
		}
		peak := float32(25)
		for _, v := range values {
			peak = max(peak, v*1.15)
		}
		// The newest reading sits at the right edge; a short history
		// starts partway across rather than being stretched.
		const span = 90
		points := make([][2]float32, len(values))
		for i, v := range values {
			x := r.X + r.W*float32(span-len(values)+i)/float32(span-1)
			points[i] = [2]float32{x, r.Y + r.H*(1-v/peak)}
		}
		pt.StrokePolyline(points, 1.5, p.accent)
	})
}

// diskBody shows what the engine's disk holds. It is measured only while
// the VM is awake, because asking wakes it.
func (s *desktop) diskBody(c *ui.Context, p palette) {
	d := s.disk
	if d == nil {
		text := tr("正在统计…", "Measuring…")
		if !s.info.awake() {
			text = tr("引擎休眠中，未统计", "Not measured while the engine sleeps")
		}
		ui.Text(c, text).FontSize(12).TextColor(p.muted)
		return
	}
	parts := []struct {
		name string
		size int64
		ink  ui.Color
	}{
		{tr("镜像", "Images"), d.images, p.accent}, {tr("卷", "Volumes"), d.volumeTotal, p.ok},
		{tr("构建缓存", "Build cache"), d.buildCache, p.warn}, {tr("容器", "Containers"), d.containers, p.muted},
	}
	var total int64
	for _, part := range parts {
		total += part.size
	}
	ui.Row(c).FillWidth().Height(10).Gap(3).Children(func() {
		for _, part := range parts {
			if part.size > 0 {
				ui.Box(c).Width(0).Grow(float32(part.size) / float32(max(1, total))).FillHeight().Radius(3).Background(part.ink)
			}
		}
	})
	for _, part := range parts {
		ui.Row(c).FillWidth().Gap(8).AlignItems(ui.Center).Children(func() {
			ui.Box(c).Size(10, 10).Radius(3).Shrink(0).Background(part.ink)
			ui.Text(c, part.name).FontSize(12.5).TextColor(p.sub).Grow(1)
			ui.Text(c, displaySize(part.size)).Font(mono).FontSize(11.5).FontFeatures("tnum").TextColor(p.text)
		})
	}
}

// busiest lists the five running containers using the most CPU.
func (s *desktop) busiest(c *ui.Context, p palette) {
	var live []container
	for _, x := range s.containers {
		if x.State == "running" && s.usage[x.ID] != nil {
			live = append(live, x)
		}
	}
	sort.SliceStable(live, func(i, j int) bool { return s.usage[live[i].ID].cpu > s.usage[live[j].ID].cpu })
	if len(live) == 0 {
		ui.Text(c, tr("没有运行中的容器", "Nothing is running")).FontSize(12).TextColor(p.muted)
		return
	}
	ui.Column(c).FillWidth().Children(func() {
		for _, x := range live[:min(5, len(live))] {
			u := s.usage[x.ID]
			b := clickable(c.Key(x.ID)).Label(tr("打开 ", "Open ") + x.Name).FillWidth().Height(36).PaddingX(6).Gap(10).Radius(8).
				AlignItems(ui.Center).Justify(ui.Start)
			if b.Hovered() {
				b.Background(p.hover)
			}
			b.Children(func() {
				dot(c, p, "running")
				cell(c, 0, 1).Children(func() { label(c, x.Name, p.text) })
				spark(c, p, u.hist, 90)
				cell(c, 56, 0).Children(func() { number(c, fmt.Sprintf("%.1f%%", u.cpu), p.text) })
				cell(c, 64, 0).Children(func() { number(c, displaySize(int64(u.memory)), p.muted) })
			})
			if b.Clicked() {
				s.open(containersPage, x.ID)
			}
		}
	})
}

// recent lists the latest container and image events of the last hour.
func (s *desktop) recent(c *ui.Context, p palette) {
	if len(s.events) == 0 {
		ui.Text(c, tr("最近一小时没有事件", "Nothing in the last hour")).FontSize(12).TextColor(p.muted)
		return
	}
	words := map[string]string{"start": tr("启动", "started"), "stop": tr("停止", "stopped"), "die": tr("退出", "exited"),
		"restart": tr("重启", "restarted"), "destroy": tr("删除", "removed"), "pull": tr("拉取", "pulled"), "delete": tr("删除", "deleted"),
		"pause": tr("暂停", "paused"), "unpause": tr("恢复", "resumed"), "oom": tr("内存不足", "out of memory"), "tag": tr("打标签", "tagged")}
	ui.Column(c).FillWidth().Children(func() {
		for i, ev := range s.events[:min(6, len(s.events))] {
			kind := "running"
			switch ev.Action {
			case "die", "oom":
				kind = "failed"
			case "stop", "destroy", "delete":
				kind = "stopped"
			}
			action := words[ev.Action]
			if action == "" {
				action = ev.Action
			}
			ui.Row(c.Key(i)).FillWidth().Height(36).PaddingX(6).Gap(10).AlignItems(ui.Center).Children(func() {
				dot(c, p, kind)
				cell(c, 0, 1).Children(func() { label(c, ev.Name, p.text) })
				label(c, action, stateColor(p, kind)).Shrink(0)
				cell(c, 76, 0).Children(func() { number(c, since(s.now, ev.Time), p.muted) })
			})
		}
	})
}
