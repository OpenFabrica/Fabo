package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"
)

// The fixtures in testdata/builds are buildx v0.38.1's real output against
// Fabo's managed BuildKit: "ok" (completed, made bh-ok:1), "bad" (RUN exit
// 3), "warn" (two build check warnings) and a classic POST /build without
// a name.
const (
	okBuild      = "9pikgnc18t84w8gapxo4cymys"
	badBuild     = "xbw3j3j082rg209cm9jje2fwh"
	warnBuild    = "nrs9gj3vg46bspf7c88p016rg"
	classicBuild = "b65k0272frvh3k6ytmr9bs4zi"
	okManifest   = "sha256:49fc11b5d82b488e72e6c3b57e8e56a30e3c8607ab5d37c2425f0b985771a4de"
	okConfig     = "sha256:f2e5ff11881fb35f3411d184f96e152c9903a45563a372531966c5f357796dc4"
)

// fakeBuildx answers buildx commands from the fixtures and records the
// ones that change something.
type fakeBuildx struct {
	t       *testing.T
	mu      sync.Mutex
	calls   [][]string
	removed []string
	// builders is `buildx ls --format json`.
	builders string
}

// fixture reads one file of testdata/builds.
func (f *fakeBuildx) fixture(name string) []byte {
	data, err := os.ReadFile(filepath.Join("testdata", "builds", name))
	if err != nil {
		f.t.Fatal(err)
	}
	return data
}

// run is a buildCLI over the fixtures.
func (f *fakeBuildx) run(_ context.Context, args ...string) ([]byte, []byte, error) {
	f.mu.Lock()
	f.calls = append(f.calls, args)
	f.mu.Unlock()
	if len(args) < 2 || args[0] != "buildx" {
		return nil, []byte("unknown command"), errors.New("exit status 1")
	}
	cmd := strings.Join(args[1:], " ")
	// The ID follows the builder's name.
	id := ""
	if i := slices.Index(args, "--builder"); i >= 0 && i+2 < len(args) {
		id = args[i+2]
	}
	switch {
	case cmd == "version":
		return []byte("github.com/docker/buildx v0.38.1"), nil, nil
	case cmd == "ls --format json":
		return []byte(f.builders), nil, nil
	case strings.HasPrefix(cmd, "history ls "):
		// buildx names a build only through the builder that ran it.
		list := f.fixture("ls.json")
		if args[slices.Index(args, "--builder")+1] != "fabo-build" {
			list = regexp.MustCompile(`"name":"[^"]*"`).ReplaceAll(list, []byte(`"name":""`))
		}
		return list, nil, nil
	case strings.HasPrefix(cmd, "history inspect attachment "):
		if args[len(args)-1] == okManifest {
			return f.fixture("manifest-" + id + ".json"), nil, nil
		}
		return f.fixture("config-" + id + ".json"), nil, nil
	case strings.HasPrefix(cmd, "history inspect "):
		return f.fixture("inspect-" + id + ".json"), nil, nil
	case strings.HasPrefix(cmd, "history logs "):
		// rawjson goes to stderr.
		return nil, f.fixture("logs-" + args[len(args)-1] + ".rawjson"), nil
	case strings.HasPrefix(cmd, "history rm "):
		f.mu.Lock()
		f.removed = append(f.removed, args[slices.Index(args, "--builder")+2:]...)
		f.mu.Unlock()
		return nil, nil, nil
	}
	return nil, []byte("ERROR: unexpected " + cmd), errors.New("exit status 1")
}

// fabo's builders as `fabo setup` leaves them, in buildx's order: fabo and
// fabo-build share a node, another context's builder and one whose
// container is gone.
const fakeBuilders = `{"Name":"fabo","Driver":"docker-container","Nodes":[{"Name":"fabo-build0","Endpoint":"fabo"}]}
{"Name":"fabo-build","Driver":"docker-container","Nodes":[{"Name":"fabo-build0","Endpoint":"fabo"}]}
{"Name":"kiri-build","Driver":"docker-container","Nodes":[{"Name":"kiri-build0","Endpoint":"kiri"}]}
{"Name":"gone","Driver":"docker-container","Nodes":[{"Name":"gone0","Endpoint":"fabo"}]}
{"Name":"default","Driver":"docker","Nodes":[{"Name":"default","Endpoint":"default"}]}
`

// newFakeHistory is a history over the fixtures whose builder container
// exists.
func newFakeHistory(t *testing.T) (*fakeBuildx, buildHistory) {
	f := &fakeBuildx{t: t, builders: fakeBuilders}
	return f, buildHistory{run: f.run, have: map[string]bool{"buildx_buildkit_fabo-build0": true, "buildx_buildkit_kiri-build0": true}, context: "fabo"}
}

func TestBuildBuildersShareNodesAndNeedTheirContainer(t *testing.T) {
	_, h := newFakeHistory(t)
	got, err := h.builders(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// The builders of one BuildKit are one group; another context's
	// builder and a builder without its container are left alone.
	if len(got) != 1 || !slices.Equal(got[0], []string{"fabo", "fabo-build"}) {
		t.Fatalf("builders = %v", got)
	}
	// No container: no builder, so buildx never creates one.
	h.have = nil
	if got, _ := h.builders(context.Background()); len(got) != 0 {
		t.Fatalf("builders without a container = %v", got)
	}
}

func TestBuildBuildersWithoutBuildx(t *testing.T) {
	h := buildHistory{run: func(context.Context, ...string) ([]byte, []byte, error) {
		return nil, []byte("docker: unknown command: docker buildx"), errors.New("exit status 1")
	}, context: "fabo"}
	items, err := h.list(context.Background())
	if err != nil || len(items) != 0 {
		t.Fatalf("list without buildx = %v, %v", items, err)
	}
}

func TestBuildList(t *testing.T) {
	_, h := newFakeHistory(t)
	items, err := h.list(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 4 {
		t.Fatalf("%d builds", len(items))
	}
	for i := 1; i < len(items); i++ {
		if items[i].Started.After(items[i-1].Started) {
			t.Fatal("builds are not newest first")
		}
	}
	byID := map[string]buildRecord{}
	for _, x := range items {
		byID[x.ID] = x
		// Both builders of the node list every build, each kept once: a
		// named one from the builder that names it.
		if x.Name != "" && x.Builder != "fabo-build" {
			t.Fatalf("%s: builder = %q", x.Name, x.Builder)
		}
	}
	if x := byID[badBuild]; x.Name != "bad" || buildKind(x.Status) != "fail" || x.Total != 7 || x.Done != 6 || x.Cached != 1 {
		t.Fatalf("bad build = %+v", x)
	}
	if x := byID[classicBuild]; x.title() != classicBuild || buildKind(x.Status) != "ok" {
		t.Fatalf("classic build = %+v", x)
	}
}

func TestBuildKind(t *testing.T) {
	for status, want := range map[string]string{"Completed": "ok", "completed": "ok", "Error": "fail", "failed": "fail", "Running": "run", "Canceled": "off", "": "off"} {
		if got := buildKind(status); got != want {
			t.Errorf("buildKind(%q) = %q, want %q", status, got, want)
		}
	}
}

func TestBuildDetailSteps(t *testing.T) {
	_, h := newFakeHistory(t)
	d, err := h.detail(context.Background(), "fabo-build", okBuild)
	if err != nil {
		t.Fatal(err)
	}
	if d.Name != "ok" || d.kind() != "ok" || d.Context != "ok" || d.Dockerfile != "Dockerfile" || d.Error != nil {
		t.Fatalf("detail = %+v", d.buildRecord)
	}
	var names []string
	for _, st := range d.Steps {
		names = append(names, st.Name)
	}
	run := slices.Index(names, "[2/4] RUN echo hello && sleep 1")
	from := slices.IndexFunc(names, func(n string) bool { return strings.HasPrefix(n, "[1/4] FROM docker.io/library/alpine:3.20@sha256:") })
	if run < 0 || from < 0 || from > run {
		t.Fatalf("steps out of order: %q", names)
	}
	st := d.Steps[run]
	if st.state() != "ok" || st.internal() || !strings.Contains(st.Log, "hello") || buildSpan(st.Started, st.Completed) < 900_000_000 {
		t.Fatalf("RUN step = %+v", st)
	}
	if !slices.ContainsFunc(d.Steps, func(st buildStep) bool { return st.internal() }) {
		t.Fatal("no internal step")
	}
}

func TestBuildDetailFailure(t *testing.T) {
	_, h := newFakeHistory(t)
	d, err := h.detail(context.Background(), "fabo-build", badBuild)
	if err != nil {
		t.Fatal(err)
	}
	if d.kind() != "fail" || d.Error == nil || d.Error.Step != "[3/4] RUN exit 3" || !strings.Contains(d.Error.Message, "exit code: 3") ||
		!strings.Contains(d.Error.Source, ">>> RUN exit 3") {
		t.Fatalf("failure = %+v", d.Error)
	}
	i := slices.IndexFunc(d.Steps, func(st buildStep) bool { return st.Name == "[3/4] RUN exit 3" })
	if i < 0 || d.Steps[i].state() != "fail" {
		t.Fatalf("failed step missing: %+v", d.Steps)
	}
	// A canceled build ends as an error saying so.
	d.Error.Message = "context canceled"
	if d.kind() != "off" {
		t.Fatal("a canceled build is not canceled")
	}
}

func TestBuildDetailWarnings(t *testing.T) {
	_, h := newFakeHistory(t)
	d, err := h.detail(context.Background(), "fabo-build", warnBuild)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Warnings) != 2 {
		t.Fatalf("warnings = %+v", d.Warnings)
	}
	w := d.Warnings[0]
	if !strings.HasPrefix(w.Text, "FromAsCasing:") || w.Line != 1 || w.Detail == "" || !strings.HasPrefix(w.URL, "https://docs.docker.com/") {
		t.Fatalf("warning = %+v", w)
	}
}

func TestBuildLayers(t *testing.T) {
	_, h := newFakeHistory(t)
	l, err := h.layers(context.Background(), "fabo-build", okBuild)
	if err != nil {
		t.Fatal(err)
	}
	if len(l.Images) != 1 || !slices.Equal(l.Bases, []string{"alpine@3.20"}) {
		t.Fatalf("layers = %+v", l)
	}
	img := l.Images[0]
	if img.ConfigDigest != okConfig || img.Platform != "linux/arm64" || img.Size != 4092319+32+249+32 {
		t.Fatalf("image = %+v", img)
	}
	var own, base []string
	for _, layer := range img.Layers {
		if layer.Base {
			base = append(base, layer.By)
		} else {
			own = append(own, layer.By)
		}
	}
	// The alpine lines are dated months before the image; the build's own
	// lines, with " # buildkit" trimmed, are not.
	if len(base) != 2 || !strings.HasPrefix(base[0], "ADD alpine-minirootfs") || own[0] != "RUN /bin/sh -c echo hello && sleep 1" {
		t.Fatalf("own %q, base %q", own, base)
	}
	cfg := img.Config
	if cfg.WorkingDir != "/app" || !slices.Equal(cfg.Ports, []string{"8080/tcp"}) || !slices.Contains(cfg.Env, "FOO=bar") {
		t.Fatalf("config = %+v", cfg)
	}
}

func TestBuildArgumentsAreChecked(t *testing.T) {
	f, h := newFakeHistory(t)
	for _, c := range []struct{ builder, id string }{{"-x", okBuild}, {"fabo-build", "--format"}, {"fabo-build", ""}, {"", okBuild}} {
		if _, err := h.detail(context.Background(), c.builder, c.id); err == nil {
			t.Errorf("detail(%q, %q) ran", c.builder, c.id)
		}
		if err := h.remove(context.Background(), c.builder, []string{c.id}); err == nil {
			t.Errorf("remove(%q, %q) ran", c.builder, c.id)
		}
	}
	if len(f.calls) != 0 {
		t.Fatalf("buildx ran: %v", f.calls)
	}
}

func TestBuildDuration(t *testing.T) {
	for d, want := range map[int64]string{400_000_000: "0.4s", 12_400_000_000: "12s", 125_000_000_000: "2m 5s"} {
		if got := buildDuration(time.Duration(d)); got != want {
			t.Errorf("buildDuration(%d) = %q, want %q", d, got, want)
		}
	}
}

// newBuildsDesktop is the fake engine's window with the fixtures' history,
// on the Builds page.
func newBuildsDesktop(t *testing.T) (*fakeBuildx, *desktop) {
	_, s := newFakeEngine(t)
	f, h := newFakeHistory(t)
	s.buildsFake = &h
	s.prefs.Lang = "en"
	s.refresh()
	s.navigate(buildsPage)
	return f, s
}

func TestBuildsPage(t *testing.T) {
	f, s := newBuildsDesktop(t)
	if !s.bl.read || len(s.bl.items) != 4 {
		t.Fatalf("builds read = %v, %d", s.bl.read, len(s.bl.items))
	}
	tt := ui.NewTester(s.view, 1280, 800)
	tt.Frame()
	tt.Frame()
	// The newest build opens with the list, as upstream's view does.
	if s.selected[buildsPage] != "fabo-build/"+warnBuild {
		t.Fatalf("selected = %q", s.selected[buildsPage])
	}
	for _, text := range []string{"warn", "bad", "FromAsCasing", "Completed"} {
		if !tt.HasText(text) {
			t.Fatalf("no %q on the page", text)
		}
	}
	// The failed build shows why.
	s.selectRow("fabo-build/" + badBuild)
	tt.Frame()
	tt.Frame()
	if !tt.HasText("did not complete successfully: exit code: 3") || !tt.HasText("Clear 1 failed") {
		t.Fatal("the failed build's reason is missing")
	}
	// Steps: only the failed one, then every step with BuildKit's own.
	s.bl.steps = 3
	tt.Frame()
	if tt.HasText("[2/4]") || !tt.HasText("[3/4]") {
		t.Fatal("the Failed filter shows a step that ran")
	}
	s.bl.steps, s.bl.internal = 0, true
	tt.Frame()
	if !tt.HasText("load build definition from Dockerfile") {
		t.Fatal("internal steps are hidden")
	}
	// A step's output opens on a click.
	s.selectRow("fabo-build/" + okBuild)
	tt.Frame()
	tt.Frame()
	if err := tt.Click("[2/4] RUN echo hello && sleep 1"); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	if !tt.HasText("hello") || !s.bl.open["[2/4] RUN echo hello && sleep 1"] {
		t.Fatal("the step's output did not open")
	}
	// Layers: the build's own work and the base image's.
	s.tab[buildsPage] = 1
	tt.Frame()
	tt.Frame()
	for _, text := range []string{"Added by this build", "From the base image", "How it runs", "FROM alpine@3.20"} {
		if !tt.HasText(text) {
			t.Fatalf("no %q on the Layers tab", text)
		}
	}
	// Security: the image isn't in the fake engine.
	s.tab[buildsPage] = 2
	tt.Frame()
	if !tt.HasText("The image isn't in Fabo") {
		t.Fatal("the Security tab did not say the image is missing")
	}
	// Remove asks, then takes the record out.
	s.confirmRemoveBuild(s.bl.items[slices.IndexFunc(s.bl.items, func(x buildRecord) bool { return x.ID == badBuild })])
	s.pending.run()
	if !slices.Equal(f.removed, []string{badBuild}) {
		t.Fatalf("removed = %v", f.removed)
	}
}

func TestBuildsPageEmpty(t *testing.T) {
	_, s := newFakeEngine(t)
	f, h := newFakeHistory(t)
	h.have = nil
	s.buildsFake = &h
	s.prefs.Lang = "en"
	s.refresh()
	s.navigate(buildsPage)
	tt := ui.NewTester(s.view, 1180, 760)
	tt.Frame()
	if !tt.HasText("No builds yet") {
		t.Fatal("no empty state")
	}
	for _, call := range f.calls {
		if slices.Contains(call, "history") {
			t.Fatalf("history read without a builder container: %v", call)
		}
	}
}
