package main

import (
	"context"
	"os"
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
	if err := tt.Click("Pack…"); err != nil {
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

// An image's Pack button writes the engine's archive of it into the chosen
// file; Import sends a chosen archive to the engine and names what loaded.
func TestImagePackAndImport(t *testing.T) {
	f, s := newFakeEngine(t)
	dir := t.TempDir()
	in := filepath.Join(dir, "in.tar")
	if err := os.WriteFile(in, []byte("an image archive"), 0o644); err != nil {
		t.Fatal(err)
	}
	var asked []string
	s.chooser = func(save bool, name string) string {
		if !save {
			return in
		}
		asked = append(asked, name)
		return filepath.Join(dir, "api")
	}
	s.navigate(imagesPage)
	tt := ui.NewTester(s.view, 1180, 760)
	click(t, tt, "Pack shop/api:dev")
	got, err := os.ReadFile(filepath.Join(dir, "api.tar"))
	if err != nil || string(got) != "archive of shop/api:dev" || !slices.Equal(asked, []string{"shop-api-dev.tar"}) {
		t.Fatalf("saved %q %v, asked %q", got, err, asked)
	}
	if leftovers, _ := filepath.Glob(filepath.Join(dir, ".*partial")); len(leftovers) > 0 {
		t.Fatalf("partial files left: %v", leftovers)
	}
	click(t, tt, "Import")
	f.mu.Lock()
	sent := f.last
	f.mu.Unlock()
	tt.Frame()
	if sent != "an image archive" || !tt.HasText("Imported shop/api:dev") {
		t.Fatalf("load sent %q; texts %q", sent, tt.Texts())
	}
}

// At every window width, with the panel open or not, the images toolbar's
// Pull image stays in view and the image's name keeps its room.
func TestImageToolbarFitsEveryWidth(t *testing.T) {
	_, s := newFakeEngine(t)
	s.navigate(imagesPage)
	for _, sel := range []string{"", "sha256:img-api"} {
		s.selectRow(sel)
		for w := 860; w <= 1500; w += 20 {
			s.lists = map[string]*ui.ListState{}
			tt := ui.NewTester(s.view, w, 760)
			tt.Frame()
			card := float32(w)
			if sel != "" {
				card -= s.dockWidth()
			}
			pull, found := tt.Find("Pull image")
			name, named := tt.Find("alpine:3.21")
			if !found || pull.X+pull.W > card || !named || name.W < 60 {
				t.Fatalf("width %v, panel %q: pull %v, name %v", w, sel, pull, name)
			}
			if _, found := tt.Find("Pack alpine:3.21"); !found && sel == "" {
				t.Fatalf("width %v: no Pack button", w)
			}
		}
	}
}
