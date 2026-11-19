package main

import (
	"cmp"
	"slices"
	"strings"

	"github.com/egoist/mygo/ui"
)

// tableLayout is what a table remembers between launches: the widths of
// its columns once the user dragged one, and the column it is sorted by.
type tableLayout struct {
	Widths map[int]float32 `json:"widths,omitempty"`
	// Sort is the sorted column's index plus one; 0 keeps the engine's order.
	Sort int  `json:"sort,omitempty"`
	Desc bool `json:"desc,omitempty"`
}

// Dragged columns stay between these widths.
const (
	columnMinWidth = 48
	columnMaxWidth = 1600
)

// layoutColumns returns cols with the widths the user dragged them to, so
// that the header and the rows of page share them. A column with a stored
// width no longer shares spare room.
func (s *desktop) layoutColumns(page string, cols []column) []column {
	out := slices.Clone(cols)
	for i, w := range s.prefs.Tables[page].Widths {
		if i >= 0 && i < len(out) {
			out[i].width, out[i].grow = w, 0
		}
	}
	return out
}

// sortedBy orders rows by the page's sorted column, keeping the engine's
// order among equals; by holds one comparison per column.
func sortedBy[T any](s *desktop, page string, rows []T, by []func(a, b T) int) {
	t := s.prefs.Tables[page]
	if t.Sort <= 0 || t.Sort > len(by) || by[t.Sort-1] == nil {
		return
	}
	less := by[t.Sort-1]
	slices.SortStableFunc(rows, func(a, b T) int {
		if t.Desc {
			return less(b, a)
		}
		return less(a, b)
	})
}

// compareFold compares text without regard to case.
func compareFold(a, b string) int {
	return cmp.Compare(strings.ToLower(a), strings.ToLower(b))
}

// setTable stores a changed layout for page.
func (s *desktop) setTable(page string, t tableLayout) {
	if s.prefs.Tables == nil {
		s.prefs.Tables = map[string]tableLayout{}
	}
	s.prefs.Tables[page] = t
}

// sortHeader is tableHeader for a table that sorts and resizes: clicking a
// title sorts by it, again reverses; dragging the right edge of a title
// moves the border to the next column. cols are the layoutColumns of page,
// hidden the indexes the rows leave out.
func (s *desktop) sortHeader(c *ui.Context, p palette, page string, cols []column, hidden []int, actions float32) {
	var shown []int
	for i := range cols {
		if !slices.Contains(hidden, i) {
			shown = append(shown, i)
		}
	}
	t := s.prefs.Tables[page]
	// Each title cell's width in the last frame, which a drag freezes.
	measured := map[int]float32{}
	// A drag is applied once every title cell has been measured.
	var dragged func()
	row := ui.Row(c.Key("head")).FillWidth().Height(38).Shrink(0).Padding(0, 14, 0, pageInset).Gap(12).AlignItems(ui.Center).
		Background(p.soft).BorderWidth(1, 0, 1, 0).BorderColor(p.line)
	// The column borders stand out while the pointer is over the header.
	showGrips := row.Hovered()
	row.Children(func() {
		for n, i := range shown {
			col := cols[i]
			head := cell(c.Key(i), col.width, col.grow).FillHeight()
			measured[i] = head.Bounds().W
			head.Children(func() {
				on := t.Sort == i+1
				ink := p.muted
				if on {
					ink = p.text
				}
				title := clickable(c.Key("sort")).Label(tr("排序：", "Sort by ") + col.title).FillHeight().MinWidth(0).Gap(4).AlignItems(ui.Center)
				if col.right {
					// Keep the title clear of the border line at the cell's edge.
					title.FillWidth().Justify(ui.End).Padding(0, 8, 0, 0)
				}
				if title.Hovered() {
					ink = p.text
				}
				title.Children(func() {
					ui.Text(c, col.title).FontSize(12).FontWeight(500).TextColor(ink).SingleLine().MinWidth(0)
					if on {
						arrow := "↑"
						if t.Desc {
							arrow = "↓"
						}
						ui.Text(c, arrow).FontSize(11).TextColor(p.accent).Shrink(0)
					}
				})
				if title.Clicked() {
					next := tableLayout{Widths: t.Widths, Sort: i + 1}
					next.Desc = on && !t.Desc
					s.setTable(page, next)
					s.savePrefs()
				}
				if n+1 < len(shown) {
					if f := s.columnGrip(c, p, page, cols, i, measured, showGrips); f != nil {
						dragged = f
					}
				}
			})
		}
		ui.Box(c).Width(actions).Shrink(0)
	})
	if dragged != nil {
		dragged()
	}
}

// columnGrip is the drag handle on the right edge of column i; dragging it
// changes only that column; its line darkens while the header is hovered,
// and turns the accent under the pointer, and a double click restores the default
// widths. The first drag freezes every shown column at
// its current width, so that columns sharing spare room stop following.
// It returns the change to apply after the header is built, when measured
// holds every shown column.
func (s *desktop) columnGrip(c *ui.Context, p palette, page string, cols []column, i int, measured map[int]float32, shown bool) func() {
	grip := ui.Box(c.Key("grip")).Absolute().Top(8).Bottom(8).Right(0).Width(6).Cursor(ui.CursorResizeColumn).
		BorderWidth(0, 1, 0, 0).BorderColor(p.line).Label(tr("调整列宽", "Resize column"))
	switch {
	case grip.Hovered() || grip.Pressed():
		grip.BorderWidth(0, 2, 0, 0).BorderColor(p.accent)
	case shown:
		grip.BorderColor(p.line.Mix(p.muted, 0.4))
	}
	var apply func()
	if dx, _, held := grip.Dragged(); held && dx != 0 {
		apply = func() {
			t := s.prefs.Tables[page]
			widths := map[int]float32{}
			for k, w := range measured {
				if w > 0 {
					widths[k] = w
				}
			}
			for k, w := range t.Widths {
				widths[k] = w
			}
			base := widths[i]
			if base == 0 {
				base = cols[i].width
			}
			widths[i] = min(columnMaxWidth, max(columnMinWidth, base+dx))
			t.Widths = widths
			s.setTable(page, t)
		}
	}
	if _, _, ended := grip.DragEnded(); ended {
		s.savePrefs()
	}
	// Double-clicking any edge gives every column its default width back.
	if grip.DoubleClicked() {
		t := s.prefs.Tables[page]
		t.Widths = nil
		s.setTable(page, t)
		s.savePrefs()
	}
	return apply
}
