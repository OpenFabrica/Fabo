package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/egoist/mygo/ui"
)

// fakeImports stands in for `fabo import` and `fabo context`: two sources,
// an inventory that follows what was imported, and docker's context.
type fakeImports struct {
	t        *testing.T
	calls    []string
	imported map[string]bool
	current  string
	running  bool
	fail     string // an item whose import fails
}

func (f *fakeImports) run(_ context.Context, args ...string) ([]byte, error) {
	switch args[0] {
	case "domains":
		return []byte(`{}`), nil
	case "context":
		if len(args) == 1 {
			return []byte(`{"current":"` + f.current + `","ours":"fabo","app":"Fabo"}`), nil
		}
		f.calls = append(f.calls, strings.Join(args, " "))
		f.current = args[2]
		return nil, nil
	case "import":
	default:
		f.t.Fatalf("unexpected launcher call %v", args)
	}
	f.calls = append(f.calls, strings.Join(args, " "))
	switch {
	case args[1] == "--sources":
		running := "false"
		if f.running {
			running = "true"
		}
		return []byte(`[{"id":"orbstack","label":"OrbStack","running":` + running + `},{"id":"desktop-linux","label":"Docker Desktop","running":false}]`), nil
	case len(args) == 3:
		have := func(k string) string {
			if f.imported[k] {
				return "true"
			}
			return "false"
		}
		return []byte(`{"source":"orbstack","images":[{"name":"nginx:latest","detail":"190MB","have":` + have("image/nginx:latest") + `},{"name":"redis:7","detail":"117MB","have":` + have("image/redis:7") + `},{"name":"alpine:latest","have":true}],` +
			`"volumes":[{"name":"pgdata","have":` + have("volume/pgdata") + `},{"name":"` + strings.Repeat("a1", 32) + `","have":false,"anonymous":true}],` +
			`"containers":[{"name":"web","detail":"nginx:latest · exited","have":` + have("container/web") + `}]}`), nil
	}
	key := args[2] + "/" + args[3]
	if key == f.fail {
		return nil, errors.New("port 80 is already allocated")
	}
	f.imported[key] = true
	return nil, nil
}

// newImportDesktop is a desktop whose launcher is a fakeImports.
func newImportDesktop(t *testing.T, current string) (*desktop, *fakeImports) {
	_, s := newFakeEngine(t)
	f := &fakeImports{t: t, imported: map[string]bool{}, current: current, running: true}
	s.launcher = f.run
	return s, f
}

// Settings → Import starts with nothing ticked, ticks a kind with Select
// all, imports the ticked items one by one, and shows how each went.
func TestImportPaneImportsTickedItemsThroughTheLauncher(t *testing.T) {
	s, f := newImportDesktop(t, "fabo")
	f.fail = "container/web"
	s.navigate(settingsPage)
	s.settingsTab = indexSettings("import")
	tt := ui.NewTester(s.view, 1180, 900)
	if m := has(tt, "nginx:latest", "190MB", "pgdata", "anonymous", "web", "In Fabo", "OrbStack isn't changed: images are copied, and volumes are read through a temporary read-only helper that's removed after. Containers arrive stopped."); m != "" {
		t.Fatalf("missing %q in %q", m, tt.Texts())
	}
	if len(s.importPicked()) != 0 {
		t.Fatalf("ticked before asked: %v", s.imp.picked)
	}
	click(t, tt, "Select all · Volumes")
	click(t, tt, "Select all · Containers")
	click(t, tt, "nginx:latest")
	if s.imp.picked["volume/"+strings.Repeat("a1", 32)] {
		t.Fatal("Select all ticked an anonymous volume")
	}
	click(t, tt, "Import 3 items")
	f.calls = f.calls[2:]
	want := "import orbstack image nginx:latest, import orbstack volume pgdata, import orbstack container web, import orbstack --json"
	if got := strings.Join(f.calls, ", "); got != want {
		t.Fatalf("calls:\n%s\nwant:\n%s", got, want)
	}
	tt.Frame()
	if m := has(tt, "Import failed", "port 80 is already allocated", "Imported 2; 1 failed."); m != "" {
		t.Fatalf("missing %q in %q", m, tt.Texts())
	}
	if got := s.importPicked(); len(got) != 0 {
		t.Fatalf("left ticked: %v", got)
	}
}

// A source that is installed but stopped is named, and nothing is read from it.
func TestImportPaneWaitsForAStoppedSource(t *testing.T) {
	s, f := newImportDesktop(t, "fabo")
	f.running = false
	s.navigate(settingsPage)
	s.settingsTab = indexSettings("import")
	tt := ui.NewTester(s.view, 1180, 900)
	if m := has(tt, "OrbStack isn't running. Start it to import from it; it doesn't need to stay open afterwards."); m != "" {
		t.Fatalf("missing %q in %q", m, tt.Texts())
	}
	if got := strings.Join(f.calls, ", "); got != "import --sources" {
		t.Fatal(got)
	}
}

// The strip under the lists says what the source has; Review opens the
// window with the images ticked, and closing the strip keeps it closed
// until the source has more.
func TestImportStripReviewAndDismiss(t *testing.T) {
	s, f := newImportDesktop(t, "fabo")
	tt := ui.NewTester(s.view, 1180, 760)
	if m := has(tt, "OrbStack has 2 images, 1 volumes and 1 containers", "that aren't in Fabo yet."); m != "" {
		t.Fatalf("missing %q in %q", m, tt.Texts())
	}
	click(t, tt, "Review")
	if m := has(tt, "Bring your OrbStack things to Fabo", "4 items aren't in Fabo yet. Pick what to copy over.", "Import 2 items", "2 of 2"); m != "" {
		t.Fatalf("missing %q in %q", m, tt.Texts())
	}
	// The sidebar's Volumes comes first by name; choose the column directly.
	s.imp.col = "volume"
	tt.Frame()
	if m := has(tt, "pgdata"); m != "" {
		t.Fatalf("missing %q in %q", m, tt.Texts())
	}
	click(t, tt, "Import 2 items")
	if !f.imported["image/nginx:latest"] || !f.imported["image/redis:7"] || f.imported["volume/pgdata"] {
		t.Fatal(f.imported)
	}
	tt.Frame()
	click(t, tt, "Done")
	if s.imp.review {
		t.Fatal("the window stayed open")
	}
	tt.Frame()
	click(t, tt, "Hide until OrbStack has more")
	tt.Frame()
	if tt.HasText("that aren't in Fabo yet.") || loadPrefs(s.prefsFile).ImportSeen["orbstack"] != 2 {
		t.Fatalf("strip still shown: %v", loadPrefs(s.prefsFile).ImportSeen)
	}
}

// While docker uses OrbStack, the strip offers to switch; switching with
// "bring all" ticked moves docker to Fabo and brings the images over, and
// Switch back returns docker and quiets the strip for a week.
func TestSwitchDockerToFaboBringsImages(t *testing.T) {
	s, f := newImportDesktop(t, "orbstack")
	tt := ui.NewTester(s.view, 1180, 760)
	if m := has(tt, "docker still uses OrbStack,"); m != "" {
		t.Fatalf("missing %q in %q", m, tt.Texts())
	}
	click(t, tt, "Switch to Fabo")
	if m := has(tt, "Use Fabo for docker?", "Bring all my OrbStack images to Fabo", "2 images, 293 MB. You can keep working while they copy."); m != "" {
		t.Fatalf("missing %q in %q", m, tt.Texts())
	}
	if !s.imp.bring {
		t.Fatal("bring all is not ticked")
	}
	f.calls = nil
	click(t, tt, "Switch to Fabo")
	want := "context use fabo, import orbstack image nginx:latest, import orbstack image redis:7, import orbstack --json"
	if got := strings.Join(f.calls, ", "); got != want {
		t.Fatalf("calls:\n%s\nwant:\n%s", got, want)
	}
	tt.Frame()
	if m := has(tt, "docker now uses Fabo", "Brought 2 items over from OrbStack.", "Show images"); m != "" {
		t.Fatalf("missing %q in %q", m, tt.Texts())
	}
	if tt.HasText("docker still uses OrbStack,") {
		t.Fatal("the switch strip stayed")
	}
	click(t, tt, "Switch back")
	if f.current != "orbstack" || s.imp.card != nil || loadPrefs(s.prefsFile).CtxSnooze == 0 {
		t.Fatalf("switch back: %q %+v", f.current, s.imp.card)
	}
	tt.Frame()
	if tt.HasText("docker still uses OrbStack,") {
		t.Fatal("the switch strip is back the same week")
	}
}

// "Choose what to bring…" opens the review window, whose button switches
// docker as it imports; Not now on the strip quiets it.
func TestSwitchByChoosingWhatToBring(t *testing.T) {
	s, f := newImportDesktop(t, "orbstack")
	tt := ui.NewTester(s.view, 1180, 760)
	click(t, tt, "Switch to Fabo")
	click(t, tt, "Choose what to bring…")
	click(t, tt, "Switch and import 2 items")
	if f.current != "fabo" || !f.imported["image/redis:7"] {
		t.Fatalf("context %q, imported %v", f.current, f.imported)
	}
	s2, _ := newImportDesktop(t, "orbstack")
	tt = ui.NewTester(s2.view, 1180, 760)
	click(t, tt, "Not now: ask again in a week")
	tt.Frame()
	if tt.HasText("docker still uses OrbStack,") {
		t.Fatal("Not now did not quiet the strip")
	}
}

func TestDockerBytes(t *testing.T) {
	for text, want := range map[string]int64{"1.2GB": 1.2e9, "187MB": 187e6, "5.6kB": 5600, "12B": 12, "n/a": 0} {
		if got := dockerBytes(text); got != want {
			t.Errorf("%s: %d, want %d", text, got, want)
		}
	}
}
