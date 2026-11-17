package buildwith

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/OpenFabrica/Fabo/engine/internal/jsonfile"
)

// places is a scratch machine: docker's files under dir/.docker.
func places(t *testing.T) Places {
	t.Helper()
	dir := t.TempDir()
	docker := filepath.Join(dir, ".docker")
	return Places{Docker: docker, Buildx: filepath.Join(docker, "buildx"), State: filepath.Join(dir, ".fabo", "build.json"), Builder: "fabo-build", Context: "fabo"}
}

// put writes a file, making its folder.
func put(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}

// get is a file's text, "" when it is missing.
func get(t *testing.T, path string) string {
	t.Helper()
	raw, _ := os.ReadFile(path)
	return string(raw)
}

// jsonOf is a file read as JSON.
func jsonOf(t *testing.T, path string) object {
	t.Helper()
	v, err := jsonfile.Read(path)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// builder makes Fabo's builder in buildx's store.
func builder(t *testing.T, p Places) {
	put(t, p.Instance(p.Builder), `{"Name":"fabo-build","Driver":"docker-container","Nodes":[{"Name":"fabo-build0","Endpoint":"fabo","DriverOpts":{"default-load":"true"}}],"Dynamic":false}`)
}

// A machine with no other engine builds with Fabo unasked; choosing Docker leaves nothing.
func TestFreshMachineBuildsWithFaboAndBackLeavesNothing(t *testing.T) {
	p := places(t)
	// Before the builder exists: chosen, taken later.
	if err := p.Apply("", true); err != nil {
		t.Fatal(err)
	}
	if p.Saved() != Fabo || p.Taken() {
		t.Fatalf("saved %q, taken %v", p.Saved(), p.Taken())
	}
	builder(t, p)
	if err := p.Apply("", true); err != nil || !p.Taken() {
		t.Fatalf("not taken: %v", err)
	}
	copy := jsonOf(t, p.Instance("fabo"))
	if copy["Name"] != "fabo" || !p.Copied(copy) {
		t.Fatalf("copy = %v", copy)
	}
	if got := jsonOf(t, p.Current()); !reflect.DeepEqual(got, object{"Key": "fabo", "Name": "fabo-build", "Global": true}) {
		t.Fatalf("current = %v", got)
	}
	if p.Ask("") {
		t.Fatal("asked with no other engine")
	}
	// Docker Desktop installed afterwards: asked once.
	if !p.Ask("Docker Desktop") {
		t.Fatal("an unasked choice is asked about once another engine appears")
	}
	if err := p.Choose(Docker); err != nil {
		t.Fatal(err)
	}
	if p.Ask("Docker Desktop") || p.Taken() {
		t.Fatal("still asking or taken after choosing Docker")
	}
	if cfg := jsonOf(t, p.Config()); len(cfg) != 0 {
		t.Fatalf("config = %v", cfg)
	}
	if jsonfile.Exists(p.Current()) {
		t.Fatal("current left behind")
	}
	// The context's builder stays: it serves `docker --context fabo build`.
	if !jsonfile.IsFile(p.Instance("fabo")) {
		t.Fatal("the context's builder went")
	}
}

// A second Fabo whose program is gone hands the builds back to the first.
func TestSecondFaboWhoseProgramIsGoneHandsBuildsBack(t *testing.T) {
	main := places(t)
	builder(t, main)
	if err := main.Choose(Fabo); err != nil {
		t.Fatal(err)
	}
	// A development build, its own state and builder, takes them.
	dev := main
	dev.State, dev.Builder, dev.Context = filepath.Join(filepath.Dir(main.State), "dev.json"), "fabo-dev-build", "fabo-dev"
	put(t, dev.Instance(dev.Builder), `{"Name":"fabo-dev-build","Nodes":[{"Name":"fabo-dev-build0","Endpoint":"fabo-dev"}]}`)
	if err := dev.Choose(Fabo); err != nil {
		t.Fatal(err)
	}
	if !dev.Taken() || main.Taken() {
		t.Fatal("the second did not take the builds")
	}
	// Its program deleted: handed back, the choice kept.
	if err := dev.GiveBack(); err != nil {
		t.Fatal(err)
	}
	if !main.Taken() {
		t.Fatal("builds do not go to the first Fabo again")
	}
	if dev.Saved() != Fabo {
		t.Fatal("the choice went")
	}
	// Started again, it takes them again.
	if err := dev.Apply("", true); err != nil || !dev.Taken() {
		t.Fatalf("not taken again: %v", err)
	}
}

// Switching back restores config.json and buildx's current builder exactly.
func TestSwitchingBackRestoresExactly(t *testing.T) {
	p := places(t)
	builder(t, p)
	put(t, p.Config(), `{"auths":{"ghcr.io":{}},"currentContext":"desktop-linux","aliases":{"foo":"bar"},"someFutureKey":{"n":12345678901234567890}}`)
	before := jsonOf(t, p.Config())
	current := `{"Key":"desktop-linux","Name":"mine","Global":false}`
	put(t, p.Current(), current)
	// Another engine: nothing happens until asked.
	if err := p.Apply("Docker Desktop", true); err != nil {
		t.Fatal(err)
	}
	if !p.Ask("Docker Desktop") || get(t, p.Current()) != current || !reflect.DeepEqual(jsonOf(t, p.Config()), before) {
		t.Fatal("took the builds beside another engine unasked")
	}
	if err := p.Choose(Fabo); err != nil || !p.Taken() {
		t.Fatalf("not taken: %v", err)
	}
	if a := jsonOf(t, p.Config())["aliases"]; !reflect.DeepEqual(a, object{"foo": "bar", "builder": "buildx"}) {
		t.Fatalf("aliases = %v", a)
	}
	// Choosing again keeps the first snapshot.
	if err := p.Choose(Fabo); err != nil {
		t.Fatal(err)
	}
	if err := p.Choose(Docker); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(jsonOf(t, p.Config()), before) || get(t, p.Current()) != current {
		t.Fatalf("not restored: %s / %s", get(t, p.Config()), get(t, p.Current()))
	}
	if !strings.Contains(get(t, p.Config()), "12345678901234567890") {
		t.Fatal("a big number was rounded")
	}
	if p.Status("Docker Desktop", true).With != Docker {
		t.Fatal("status does not say docker")
	}
}

// A `builder` alias of the user's own, or an empty `aliases`, comes back.
func TestAnAliasOfTheUsersOwnComesBack(t *testing.T) {
	p := places(t)
	builder(t, p)
	put(t, p.Config(), `{"aliases":{"builder":"mybuilder"}}`)
	if err := p.Choose(Fabo); err != nil {
		t.Fatal(err)
	}
	if err := p.Choose(Docker); err != nil {
		t.Fatal(err)
	}
	if got := jsonOf(t, p.Config()); !reflect.DeepEqual(got, object{"aliases": object{"builder": "mybuilder"}}) {
		t.Fatalf("config = %v", got)
	}
	// An empty `aliases` that was there stays.
	put(t, p.Config(), `{"aliases":{}}`)
	if err := p.Choose(Fabo); err != nil {
		t.Fatal(err)
	}
	if err := p.Choose(Docker); err != nil {
		t.Fatal(err)
	}
	if got := jsonOf(t, p.Config()); !reflect.DeepEqual(got, object{"aliases": object{}}) {
		t.Fatalf("config = %v", got)
	}
}

// A `docker buildx use` made since is never fought.
func TestAChangeMadeSinceIsLeftAlone(t *testing.T) {
	p := places(t)
	builder(t, p)
	if err := p.Choose(Fabo); err != nil {
		t.Fatal(err)
	}
	// `docker buildx use other` afterwards: Apply doesn't take it back.
	theirs := `{"Key":"fabo","Name":"other","Global":false}`
	put(t, p.Current(), theirs)
	if err := p.Apply("", true); err != nil || get(t, p.Current()) != theirs {
		t.Fatalf("fought the user's builder: %v", err)
	}
	// Back to Docker: their builder stays, the alias goes.
	if err := p.Choose(Docker); err != nil {
		t.Fatal(err)
	}
	if get(t, p.Current()) != theirs || len(jsonOf(t, p.Config())) != 0 {
		t.Fatalf("current %s, config %s", get(t, p.Current()), get(t, p.Config()))
	}
}

// A builder named after the context that is not Fabo's copy stays.
func TestABuilderOfAnotherNameFaboIsNotReplaced(t *testing.T) {
	p := places(t)
	builder(t, p)
	foreign := `{"Name":"fabo","Driver":"remote","Nodes":[{"Name":"fabo0","Endpoint":"tcp://x:1234"}]}`
	put(t, p.Instance("fabo"), foreign)
	if err := p.Choose(Docker); err != nil {
		t.Fatal(err)
	}
	if get(t, p.Instance("fabo")) != foreign {
		t.Fatal("replaced a builder that is not Fabo's copy")
	}
	if err := p.Uninstall(); err != nil || get(t, p.Instance("fabo")) != foreign {
		t.Fatalf("uninstall removed it: %v", err)
	}
}

// Uninstalling gives everything back and removes Fabo's files.
func TestUninstallGivesEverythingBack(t *testing.T) {
	p := places(t)
	builder(t, p)
	if err := p.Apply("", true); err != nil || !p.Taken() {
		t.Fatal(err)
	}
	if err := p.Uninstall(); err != nil {
		t.Fatal(err)
	}
	if len(jsonOf(t, p.Config())) != 0 || jsonfile.Exists(p.Current()) || jsonfile.Exists(p.Instance("fabo")) || jsonfile.Exists(p.State) {
		t.Fatal("uninstall left something")
	}
	// Twice is harmless: the MSI may run it after a failed attempt.
	if err := p.Uninstall(); err != nil {
		t.Fatal(err)
	}
}

// Windows PowerShell writes UTF-8 with a BOM. Such a config is read, its
// unknown keys survive the round trip, and what is written back is
// BOM-free, which is what the docker CLI can parse.
func TestAConfigWithABOMKeepsItsKeys(t *testing.T) {
	p := places(t)
	builder(t, p)
	put(t, p.Config(), "\xEF\xBB\xBF"+`{"auths":{"registry.example":{"auth":"eDp5"}},"credsStore":"wincred","cliPluginsExtraDirs":["C:\\Fabo\\bin\\cli-plugins"],"plugins":{"x":{"y":"z"}}}`)
	before := jsonOf(t, p.Config())
	if err := p.Choose(Fabo); err != nil || !p.Taken() {
		t.Fatalf("not taken: %v", err)
	}
	if strings.HasPrefix(get(t, p.Config()), "\xEF\xBB\xBF") {
		t.Fatal("wrote a BOM, which docker cannot parse")
	}
	if err := p.Uninstall(); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(jsonOf(t, p.Config()), before) {
		t.Fatalf("config = %s", get(t, p.Config()))
	}
}

// A config that is not JSON is never overwritten, and nothing is taken.
func TestAnUnreadableConfigIsLeftAlone(t *testing.T) {
	p := places(t)
	builder(t, p)
	put(t, p.Config(), `{"auths":`)
	if err := p.Choose(Fabo); err == nil {
		t.Fatal("took the builds over an unreadable config")
	}
	if get(t, p.Config()) != `{"auths":` || jsonfile.Exists(p.Current()) {
		t.Fatal("changed something")
	}
	if err := p.Choose("podman"); err == nil {
		t.Fatal("accepted an unknown choice")
	}
}

// Status before anyone chose says what Apply would do.
func TestStatusBeforeAnyChoice(t *testing.T) {
	p := places(t)
	if s := p.Status("", true); s.With != Fabo || s.Ask || s.Ready || s.Builder != "fabo-build" {
		t.Fatalf("no other engine: %+v", s)
	}
	if s := p.Status("Docker Desktop", true); s.With != Docker || !s.Ask || s.Engine != "Docker Desktop" {
		t.Fatalf("another engine: %+v", s)
	}
	if s := p.Status("", false); s.With != Docker {
		t.Fatalf("a flavored app: %+v", s)
	}
}

// Windows finds docker's files as the docker CLI does.
func TestWindowsPlaces(t *testing.T) {
	env := map[string]string{"USERPROFILE": `C:\Users\me`}
	getenv := func(k string) string { return env[k] }
	p, err := WindowsPlaces(getenv, filepath.Join("fabo-dir"))
	if err != nil {
		t.Fatal(err)
	}
	if p.Docker != filepath.Join(`C:\Users\me`, ".docker") || p.Buildx != filepath.Join(p.Docker, "buildx") || p.State != filepath.Join("fabo-dir", "build.json") || p.Builder != "fabo-build" || p.Context != "fabo" {
		t.Fatalf("places = %+v", p)
	}
	env["DOCKER_CONFIG"], env["BUILDX_CONFIG"] = `D:\docker`, `D:\buildx`
	if p, _ = WindowsPlaces(getenv, "x"); p.Docker != `D:\docker` || p.Buildx != `D:\buildx` {
		t.Fatalf("env not followed: %+v", p)
	}
	if _, err := WindowsPlaces(func(string) string { return "" }, "x"); err == nil {
		t.Fatal("no USERPROFILE must fail")
	}
}

// Windows names another engine by what is installed or holds docker_engine.
func TestWindowsOtherEngine(t *testing.T) {
	env := map[string]string{"ProgramFiles": `C:\Program Files`, "LOCALAPPDATA": `C:\Users\me\AppData\Local`}
	getenv := func(k string) string { return env[k] }
	installed := map[string]bool{}
	isFile := func(p string) bool { return installed[p] }
	// Nothing installed, and docker_engine Fabo's own or free: no other engine.
	if got := WindowsOtherEngine(getenv, isFile, ""); got != "" {
		t.Fatalf("got %q", got)
	}
	if got := WindowsOtherEngine(getenv, isFile, WindowsEngineName("dockerd.exe")); got != "Docker" {
		t.Fatalf("pipe held: %q", got)
	}
	installed[filepath.Join(env["LOCALAPPDATA"], `Programs\Rancher Desktop\Rancher Desktop.exe`)] = true
	if got := WindowsOtherEngine(getenv, isFile, ""); got != "Rancher Desktop" {
		t.Fatalf("got %q", got)
	}
	installed[filepath.Join(env["ProgramFiles"], `Docker\Docker\Docker Desktop.exe`)] = true
	if got := WindowsOtherEngine(getenv, isFile, ""); got != "Docker Desktop" {
		t.Fatalf("got %q", got)
	}
	for process, want := range map[string]string{"": "", "Docker Desktop.exe": "Docker Desktop", "com.docker.backend.exe": "Docker Desktop", "Rancher Desktop.exe": "Rancher Desktop", "dockerd.exe": "Docker"} {
		if got := WindowsEngineName(process); got != want {
			t.Errorf("%q: %q", process, got)
		}
	}
}
