package main

import (
	"testing"

	"github.com/egoist/mygo/ui"
)

// TestColumnDragChangesOnlyThatColumn drags each border of the volumes
// table and checks that only the column left of it changes width.
func TestColumnDragChangesOnlyThatColumn(t *testing.T) {
	_, s := newFakeEngine(t)
	s.prefs.Lang = "zh"
	s.prefsFile = t.TempDir() + "/desktop.json"
	s.page = volumesPage
	tt := ui.NewTester(s.view, 1180, 760)
	tt.Frame()
	head, _ := tt.Find("名称")
	drv, _ := tt.Find("驱动")
	t.Logf("name %v driver %v", head, drv)
	// The name column's grip sits just left of the gap before Driver.
	x, y := drv.X-12-3, head.Y+head.H/2
	tt.Press(x, y)
	for i := 1; i <= 10; i++ {
		tt.Move(x+float32(i*5), y)
		tt.Frame()
	}
	tt.Release(x+50, y)
	tt.Frame()
	t.Logf("widths %v", s.prefs.Tables[volumesPage].Widths)
	if w := s.prefs.Tables[volumesPage].Widths; w[1] != 80 || w[2] != 86 || w[4] != 86 {
		t.Fatalf("other columns changed: %v", w)
	}
	drv2, _ := tt.Find("驱动")
	if d := drv2.X - drv.X; d < 45 || d > 55 {
		t.Fatalf("driver moved %v, want 50", d)
	}
}

// TestAutoScanReadsEveryImagesLastScan checks that listing the images looks
// up each one's cached scan, so the table shows security without a click.
func TestAutoScanReadsEveryImagesLastScan(t *testing.T) {
	_, s := newFakeEngine(t)
	s.page = imagesPage
	tt := ui.NewTester(s.view, 1180, 760)
	tt.Frame()
	if len(s.images) == 0 || len(s.scans) != len(s.images) {
		t.Fatalf("scans %d for %d images", len(s.scans), len(s.images))
	}
	if s.autoScanning {
		t.Fatal("auto scan still marked busy")
	}
}
