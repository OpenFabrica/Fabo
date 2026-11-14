package main

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/egoist/mygo/ui"
)

// palette is the window's color set; one exists per appearance.
type palette struct {
	bg, panel, raised, line, hover, text, muted ui.Color
	link, ok, err, warn, accent, onAccent       ui.Color
}

// accentChoice is one accent Settings offers, as a tone for the dark
// palette and one for the light. Both carry white text. The first is the
// default, Kiri's blue.
type accentChoice struct{ id, zh, en, dark, light string }

var accents = []accentChoice{
	{"blue", "蓝色", "Blue", "#0D6EFD", "#0D6EFD"},
	{"purple", "紫色", "Purple", "#8C6EF5", "#6B4FD8"},
	{"teal", "青色", "Teal", "#14B4A5", "#0B7F74"},
	{"pink", "粉色", "Pink", "#EC408C", "#C71F6C"},
	{"green", "绿色", "Green", "#28AA5A", "#1B7F45"},
}

// accentColor is the chosen accent's tone for an appearance; an unknown
// name falls back to the first choice.
func accentColor(id string, dark bool) ui.Color {
	choice := accents[0]
	for _, a := range accents {
		if a.id == id {
			choice = a
		}
	}
	if dark {
		return ui.Hex(choice.dark)
	}
	return ui.Hex(choice.light)
}

// colors returns the dark or light palette with the chosen accent.
func colors(dark bool, accent string) palette {
	if dark {
		return palette{
			bg: ui.Hex("#0D1014"), panel: ui.Hex("#12161C"), raised: ui.Hex("#1A2028"), line: ui.Hex("#232A33"),
			hover: ui.Hex("#1F2630"), text: ui.Hex("#E8E5DE"), muted: ui.Hex("#8B94A1"),
			link: ui.Hex("#86A6FF"), ok: ui.Hex("#3FB68B"), err: ui.Hex("#F0564A"), warn: ui.Hex("#FFC23D"),
			accent: accentColor(accent, true), onAccent: ui.Hex("#FFFFFF"),
		}
	}
	return palette{
		bg: ui.Hex("#F3F4F6"), panel: ui.Hex("#FFFFFF"), raised: ui.Hex("#E9ECF0"), line: ui.Hex("#D8DCE3"),
		hover: ui.Hex("#E4E7EC"), text: ui.Hex("#14181E"), muted: ui.Hex("#5C6572"),
		link: ui.Hex("#1438D6"), ok: ui.Hex("#177F52"), err: ui.Hex("#C5342B"), warn: ui.Hex("#8A5F00"),
		accent: accentColor(accent, false), onAccent: ui.Hex("#FFFFFF"),
	}
}

// prefs are the window's own settings, kept beside the engine's state.
type prefs struct {
	Theme        string  `json:"theme"`   // system, light, dark
	Lang         string  `json:"lang"`    // system or a supported BCP 47 tag; legacy zh is accepted
	Density      string  `json:"density"` // compact, cozy
	Accent       string  `json:"accent"`  // one of accents
	Font         string  `json:"font"`    // interface font family; empty for the system's
	Mono         string  `json:"mono"`    // monospace family; empty for the system's
	Scale        float32 `json:"scale"`   // text size of the whole window, 1 for 100%
	LogSize      float32 `json:"log_size"`
	NavCollapsed bool    `json:"nav_collapsed"`
	DockHeight   float32 `json:"dock_height"`
}

// prefsPath places the file in the installation's state directory, which is
// two levels above the socket the launcher passes, so that each flavor keeps
// its own. Without a socket it falls back to the user's config directory.
func prefsPath() string {
	if socket := os.Getenv("KIRI_SOCKET"); socket != "" {
		return filepath.Join(filepath.Dir(filepath.Dir(socket)), "desktop.json")
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "kiri", "desktop.json")
}

// defaultPrefs are the settings of a first launch.
func defaultPrefs() prefs {
	return prefs{Theme: "system", Lang: "system", Density: "compact", Accent: "blue", Scale: 1, LogSize: 11, DockHeight: 232}
}

// loadPrefs reads the saved settings, filling anything missing with defaults.
func loadPrefs(path string) prefs {
	p := defaultPrefs()
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, &p)
	}
	if p.DockHeight < 140 {
		p.DockHeight = 232
	}
	if p.Scale < 0.5 || p.Scale > 2 {
		p.Scale = 1
	}
	if p.LogSize < 9 || p.LogSize > 24 {
		p.LogSize = 11
	}
	return p
}

// save writes the settings; a failure only costs the next launch its memory.
func (p prefs) save(path string) {
	if path == "" {
		return
	}
	if data, err := json.MarshalIndent(p, "", "  "); err == nil {
		_ = os.WriteFile(path, data, 0o600)
	}
}

// lineIcon wraps a path in the stroke settings every icon shares.
func lineIcon(shape string) *ui.SVG {
	return ui.MustParseSVG([]byte(`<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round">` + shape + `</svg>`))
}

//go:embed fabo-mark.png
var markPNG []byte

var markIcon = func() *ui.Bitmap {
	icon, err := ui.DecodeBitmap(markPNG)
	if err != nil {
		panic(err)
	}
	return icon
}()

// icons are the line icons of the design, on a 24 unit grid.
var icons = map[string]*ui.SVG{
	"grid":     lineIcon(`<rect x="4" y="4" width="7" height="7" rx="1.5"/><rect x="13" y="4" width="7" height="7" rx="1.5"/><rect x="4" y="13" width="7" height="7" rx="1.5"/><rect x="13" y="13" width="7" height="7" rx="1.5"/>`),
	"box":      lineIcon(`<path d="M12 3l8 4.5v9L12 21l-8-4.5v-9L12 3zM4 7.5l8 4.5 8-4.5M12 12v9"/>`),
	"layers":   lineIcon(`<path d="M12 4l9 5-9 5-9-5 9-5zM3 14l9 5 9-5"/>`),
	"cylinder": lineIcon(`<path d="M5 6c0-1.7 3.1-3 7-3s7 1.3 7 3v12c0 1.7-3.1 3-7 3s-7-1.3-7-3V6zM5 6c0 1.7 3.1 3 7 3s7-1.3 7-3M5 12c0 1.7 3.1 3 7 3s7-1.3 7-3"/>`),
	"network":  lineIcon(`<rect x="9" y="3" width="6" height="5" rx="1"/><rect x="3" y="16" width="6" height="5" rx="1"/><rect x="15" y="16" width="6" height="5" rx="1"/><path d="M12 8v4M6 16v-4h12v4"/>`),
	"hammer":   lineIcon(`<path d="M13.5 4.5l6 6-2.5 2.5-6-6zM12.5 9L4 17.5 6.5 20 15 11.5"/>`),
	"sliders":  lineIcon(`<path d="M4 7h9M19 7h1M4 17h1M11 17h9"/><circle cx="16" cy="7" r="2.5"/><circle cx="8" cy="17" r="2.5"/>`),
	"helm":     lineIcon(`<circle cx="12" cy="12" r="8"/><circle cx="12" cy="12" r="2.5"/><path d="M12 4v5.5M12 14.5V20M4 12h5.5M14.5 12H20M6.3 6.3l3.9 3.9M13.8 13.8l3.9 3.9M17.7 6.3l-3.9 3.9M10.2 13.8l-3.9 3.9"/>`),
	"usb":      lineIcon(`<path d="M12 3v14M9 6l3-3 3 3M8 9v3l4 3M16 8v3l-4 3"/><circle cx="12" cy="19" r="2"/>`),
	"share":    lineIcon(`<path d="M12 15V4M8 8l4-4 4 4M5 13v6h14v-6"/>`),
	"play":     lineIcon(`<path d="M8 5.5l10 6.5-10 6.5z"/>`),
	"stop":     lineIcon(`<rect x="7" y="7" width="10" height="10" rx="1.5"/>`),
	"restart":  lineIcon(`<path d="M19 12a7 7 0 11-2.6-5.4M19 4v4.5h-4.5"/>`),
	"trash":    lineIcon(`<path d="M5 7h14M9 7V4h6v3M7 7l1 13h8l1-13"/>`),
	"more":     lineIcon(`<path d="M6 12h.01M12 12h.01M18 12h.01" stroke-width="3"/>`),
	"search":   lineIcon(`<circle cx="11" cy="11" r="6"/><path d="M20 20l-4.5-4.5"/>`),
	"terminal": lineIcon(`<path d="M5 7l5 5-5 5M12 17h7"/>`),
	"chevron":  lineIcon(`<path d="M9 6l6 6-6 6"/>`),
	"down":     lineIcon(`<path d="M6 9l6 6 6-6"/>`),
	"plus":     lineIcon(`<path d="M12 5v14M5 12h14"/>`),
	"out":      lineIcon(`<path d="M10 5H5v14h14v-5M14 5h5v5M19 5l-8 8"/>`),
	"x":        lineIcon(`<path d="M6 6l12 12M18 6L6 18"/>`),
	"check":    lineIcon(`<path d="M5 12.5l4.5 4.5L19 7.5"/>`),
	"copy":     lineIcon(`<rect x="8" y="8" width="12" height="12" rx="2"/><path d="M16 8V5a1 1 0 00-1-1H5a1 1 0 00-1 1v10a1 1 0 001 1h3"/>`),
	"folder":   lineIcon(`<path d="M3 7a1 1 0 011-1h5l2 2h8a1 1 0 011 1v9a1 1 0 01-1 1H4a1 1 0 01-1-1V7z"/>`),
	"file":     lineIcon(`<path d="M7 3h7l4 4v14H7zM14 3v4h4"/>`),
	"download": lineIcon(`<path d="M12 4v11M8 11l4 4 4-4M5 20h14"/>`),
	"shield":   lineIcon(`<path d="M12 3l7 3v5c0 4.5-3 8-7 10-4-2-7-5.5-7-10V6l7-3z"/>`),
	"expand":   lineIcon(`<path d="M5 15l7-7 7 7"/>`),
	"collapse": lineIcon(`<path d="M5 9l7 7 7-7"/>`),
	"stack":    lineIcon(`<path d="M4 7h16M4 12h16M4 17h16"/>`),
	"sidebar":  lineIcon(`<rect x="3.5" y="5" width="17" height="14" rx="2.5"/><path d="M9.5 5v14"/>`),
	"power":    lineIcon(`<path d="M12 3v9M7 6.5a7.5 7.5 0 1010 0"/>`),
	"up":       lineIcon(`<path d="M12 19V5M6 11l6-6 6 6"/>`),
	"palette":  lineIcon(`<path d="M12 3a9 9 0 100 18c1.4 0 2-1 2-2 0-1.6-1-1.8-1-3 0-1 .8-1.8 2-1.8h2.2A3.8 3.8 0 0021 10.4C21 6.3 17 3 12 3z"/><path d="M7.5 12h.01M9.5 8h.01M14 7h.01" stroke-width="2.6"/>`),
	"cpu":      lineIcon(`<rect x="6" y="6" width="12" height="12" rx="2"/><rect x="10" y="10" width="4" height="4" rx="0.5"/><path d="M9 3v3M15 3v3M9 18v3M15 18v3M3 9h3M3 15h3M18 9h3M18 15h3"/>`),
	"globe":    lineIcon(`<circle cx="12" cy="12" r="9"/><path d="M3 12h18M12 3c2.6 2.6 3.8 5.6 3.8 9s-1.2 6.4-3.8 9c-2.6-2.6-3.8-5.6-3.8-9S9.4 5.600 12 3z"/>`),
	"link":     lineIcon(`<path d="M10 14a4.5 4.5 0 006.4 0l3-3a4.500 4.500 0 00-6.400-6.400l-1.200 1.200M14 10a4.500 4.500 0 00-6.400 0l-3 3a4.500 4.500 0 006.400 6.400l1.200-1.200"/>`),
	"info":     lineIcon(`<circle cx="12" cy="12" r="9"/><path d="M12 11v6M12 7.5h.01"/>`),
}

// mono is the monospace family of tables, IDs and logs. The view sets it
// from the preferences before building.
var mono = "monospace"

// clickable is the base of everything the pointer can act on: a button that
// shows the pointing hand over it.
func clickable(c *ui.Context) ui.Element {
	return ui.ButtonBase(c).Cursor(ui.CursorPointer)
}

// pointerFor is the cursor of a button that may be disabled: the hand only
// while a click would do something.
func pointerFor(disabled bool) ui.Cursor {
	if disabled {
		return ui.CursorDefault
	}
	return ui.CursorPointer
}

// iconButton is a small square button showing only an icon. Its label names
// it for assistive technology and for tests.
func iconButton(c *ui.Context, p palette, icon, label string, disabled bool) ui.Element {
	b := ui.ButtonBase(c).Label(label).Tooltip(label).Size(24, 24).Shrink(0).Center().Radius(4).Disabled(disabled).Cursor(pointerFor(disabled))
	ink := p.muted
	if b.Hovered() && !disabled {
		b.Background(p.hover)
		ink = p.text
	}
	if disabled {
		ink = p.muted.Alpha(0.4)
	}
	b.Children(func() { ui.Icon(c, icons[icon]).Size(15, 15).TextColor(ink) })
	return b
}

const (
	plainButton = iota
	primaryButton
	ghostButton
	dangerButton
)

// button is the text button of toolbars and dialogs, in one of four kinds.
func button(c *ui.Context, p palette, label, icon string, kind int, disabled bool) bool {
	b := ui.ButtonBase(c).Label(label).Height(26).PaddingX(11).Gap(5).Shrink(0).
		AlignItems(ui.Center).Justify(ui.Center).Radius(5).Disabled(disabled).Cursor(pointerFor(disabled))
	bg, ink, line := p.panel, p.text, p.line
	switch kind {
	case primaryButton:
		bg, ink, line = p.accent, p.onAccent, p.accent
	case dangerButton:
		bg, ink, line = p.err, ui.Hex("#FFFFFF"), p.err
	case ghostButton:
		bg, ink, line = ui.Transparent, p.muted, ui.Transparent
	}
	if b.Hovered() && !disabled {
		switch kind {
		case plainButton:
			bg = p.hover
		case ghostButton:
			ink = p.text
		default:
			bg = bg.Mix(p.text, 0.1)
		}
	}
	if disabled {
		// A disabled button keeps its shape but drops its color, so the
		// label stays readable on either appearance.
		bg, ink, line = p.raised.Alpha(0.6), p.muted.Alpha(0.7), p.line
		if kind == ghostButton {
			bg, line = ui.Transparent, ui.Transparent
		}
	}
	b.Background(bg).Border(1, line).Children(func() {
		if icon != "" {
			ui.Icon(c, icons[icon]).Size(13, 13).TextColor(ink)
		}
		weight := 500
		if kind == primaryButton || kind == dangerButton {
			weight = 600
		}
		ui.Text(c, label).FontSize(12).FontWeight(weight).TextColor(ink)
	})
	return b.Clicked()
}

// pill is one choice of a filter row, with the number of rows it shows.
func pill(c *ui.Context, p palette, label string, count int, on bool) bool {
	b := clickable(c).Label(label).Height(24).PaddingX(10).Gap(5).Shrink(0).Radius(12).AlignItems(ui.Center)
	bg, ink, line := ui.Transparent, p.muted, p.line
	if on {
		bg, ink, line = p.text, p.bg, p.text
	} else if b.Hovered() {
		ink = p.text
	}
	b.Background(bg).Border(1, line).Children(func() {
		weight := 400
		if on {
			weight = 600
		}
		ui.Text(c, label).FontSize(12).FontWeight(weight).TextColor(ink)
		ui.Textf(c, "%d", count).FontSize(12).FontFeatures("tnum").TextColor(ink)
	})
	return b.Clicked()
}

// dot is the status bullet: a filled circle, or a ring for something stopped.
func dot(c *ui.Context, p palette, kind string) {
	d := ui.Box(c).Size(7, 7).Shrink(0).Radius(3.5)
	switch kind {
	case "running":
		d.Background(p.ok)
	case "failed":
		d.Background(p.err)
	case "busy":
		d.Background(p.warn)
	default:
		d.Border(1.5, p.muted)
	}
}

// stateColor is the text color matching a dot.
func stateColor(p palette, kind string) ui.Color {
	switch kind {
	case "running":
		return p.ok
	case "failed":
		return p.err
	case "busy":
		return p.warn
	}
	return p.muted
}

// spark draws a short CPU history as bars scaled to their own peak, with a
// floor so that an idle container stays flat rather than looking busy.
func spark(c *ui.Context, p palette, values []float32, width float32) {
	box := ui.Box(c).Size(width, 14).Shrink(0)
	if len(values) == 0 {
		return
	}
	box.Draw(func(pt *ui.Painter, r ui.Rect) {
		peak := float32(4)
		for _, v := range values {
			peak = max(peak, v)
		}
		n := int(r.W / 4.5)
		shown := values[max(0, len(values)-n):]
		for i, v := range shown {
			h := max(2, v/peak*r.H)
			pt.Fill(ui.Rect{X: r.X + float32(i)*4.5, Y: r.Y + r.H - h, W: 3, H: h}, p.accent, 1)
		}
	})
}

// meter is a thin horizontal bar filled to a fraction between 0 and 1.
func meter(c *ui.Context, p palette, fraction float64, height float32, ink ui.Color) {
	fraction = min(1, max(0, fraction))
	ui.Box(c).FillWidth().Height(height).Shrink(0).Radius(height / 2).Background(p.raised).Clip().Children(func() {
		ui.Box(c).WidthPercent(float32(fraction * 100)).FillHeight().Radius(height / 2).Background(ink)
	})
}

// cell is one table cell: fixed in width, or sharing the row's spare room
// when width is 0. Text in it stays on one line.
func cell(c *ui.Context, width float32, grow float32) ui.Element {
	e := ui.Row(c).AlignItems(ui.Center).MinWidth(0).Clip()
	if width > 0 {
		return e.Width(width).Shrink(0)
	}
	return e.Width(0).Grow(grow)
}

// label writes table text in the monospace face at the table's size.
func label(c *ui.Context, text string, ink ui.Color) ui.Element {
	return ui.Text(c, text).Font(mono).FontSize(11.5).TextColor(ink).SingleLine().MinWidth(0)
}

// number writes a right-aligned figure filling its cell.
func number(c *ui.Context, text string, ink ui.Color) {
	ui.Text(c, text).Font(mono).FontSize(11.5).FontFeatures("tnum").TextColor(ink).SingleLine().FillWidth().TextAlign(ui.End)
}

// empty is the centered state of a list with nothing to show.
func empty(c *ui.Context, p palette, icon, title, action string) bool {
	clicked := false
	ui.Column(c).Fill().Grow(1).MinHeight(120).Center().Gap(12).Children(func() {
		ui.Icon(c, icons[icon]).Size(30, 30).TextColor(p.muted)
		ui.Text(c, title).FontSize(13).FontWeight(500).TextColor(p.text)
		if action != "" {
			clicked = button(c.Key("empty-action"), p, action, "", plainButton, false)
		}
	})
	return clicked
}

// displaySize formats a byte count with binary units, as docker does.
func displaySize(size int64) string {
	if size < 0 {
		return "—"
	}
	if size < 1024 {
		return fmt.Sprintf("%d B", size)
	}
	value := float64(size)
	for _, unit := range []string{"KB", "MB", "GB", "TB"} {
		value /= 1024
		if value < 1024 {
			if value >= 100 || unit == "KB" {
				return fmt.Sprintf("%.0f %s", value, unit)
			}
			return fmt.Sprintf("%.1f %s", value, unit)
		}
	}
	return fmt.Sprintf("%.0f PB", value/1024)
}

// ago words a span of seconds as its largest unit.
func ago(seconds int64) string {
	switch {
	case seconds < 60:
		return tr("不到 1 分钟", "under a minute")
	case seconds < 3600:
		return fmt.Sprintf(tr("%d 分钟", "%d min"), seconds/60)
	case seconds < 86400:
		return fmt.Sprintf(tr("%d 小时", "%d h"), seconds/3600)
	}
	return fmt.Sprintf(tr("%d 天", "%d d"), seconds/86400)
}

// since words how long ago a moment was, for created and finished columns.
func since(now, at int64) string {
	if at <= 0 {
		return "—"
	}
	if now-at < 60 {
		return tr("刚刚", "just now")
	}
	return ago(now-at) + tr("前", " ago")
}
