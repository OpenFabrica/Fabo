package main

import (
	"context"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/egoist/mygo/ui"
)

// A container's menu, the picked containers' bar and a project's panel pack
// through the launcher into the file the user chose; Import unpacks the
// chosen file and reports what the launcher said.
func TestPackAndImportUseLauncher(t *testing.T) {
	_, s := newFakeEngine(t)
	dir := t.TempDir()
	var calls [][]string
	var asked []string
	s.chooser = func(save bool, name string) string {
		if !save {
			asked = append(asked, "open")
			return filepath.Join(dir, "in.fabopack")
		}
		asked = append(asked, name)
		return filepath.Join(dir, strings.TrimSuffix(name, packExtension))
	}
	s.launcher = func(_ context.Context, args ...string) ([]byte, error) {
		if len(args) > 0 && (args[0] == "pack" || args[0] == "unpack") {
			calls = append(calls, slices.Clone(args))
		}
		if args[0] == "unpack" {
			return []byte("loading 2 images\nshop-api-1: /src/shop is not on this machine; mounting nothing there\nunpacked 2 containers, 1 volumes\n"), nil
		}
		return nil, nil
	}
	tt := ui.NewTester(s.view, 1180, 760)

	click(t, tt, "More actions for redis-cache")
	if err := tt.ChooseMenuItem("Pack…"); err != nil {
		t.Fatal(err)
	}
	tt.Frame()

	s.picked["c-web"], s.picked["c-api"] = true, true
	tt.Frame()
	click(t, tt, "Pack")
	clear(s.picked)

	s.selectRow("project:shop")
	tt.Frame()
	click(t, tt, "Pack the project")

	s.selectRow("")
	tt.Frame()
	click(t, tt, "Import")

	want := [][]string{
		{"pack", "-o", filepath.Join(dir, "redis-cache.fabopack"), "redis-cache"},
		{"pack", "-o", filepath.Join(dir, "shop.fabopack"), "shop-api-1", "shop-web-1"},
		{"pack", "-o", filepath.Join(dir, "shop.fabopack"), "--project", "shop"},
		{"unpack", "--start", filepath.Join(dir, "in.fabopack")},
	}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("launcher calls:\n%q\nwant\n%q", calls, want)
	}
	if !reflect.DeepEqual(asked, []string{"redis-cache.fabopack", "shop.fabopack", "shop.fabopack", "open"}) {
		t.Fatalf("dialogs: %q", asked)
	}
	// The view turns notices into toasts.
	tt.Frame()
	if !tt.HasText("Imported 2 containers") || !tt.HasText("/src/shop is not on this machine") {
		t.Fatal("the import's outcome is not shown")
	}

	// A canceled dialog runs nothing.
	s.chooser = func(bool, string) string { return "" }
	calls = nil
	click(t, tt, "Import")
	if len(calls) != 0 {
		t.Fatalf("canceled import ran %q", calls)
	}
}
