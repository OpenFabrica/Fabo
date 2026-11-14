package main

import (
	"bytes"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// sandbox is a setup whose HOME, docker directory and machine-wide paths
// all live in a temporary directory, with an app bundle to link from.
func sandbox(t *testing.T, id string) *setup {
	t.Helper()
	// Unix sockets need a short path, which t.TempDir is not on macOS.
	base, err := os.MkdirTemp("/tmp", "fabo-setup-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(base) })
	home, root := filepath.Join(base, "home"), filepath.Join(base, "root")
	contents := filepath.Join(base, "Fabo.app", "Contents")
	for _, dir := range []string{home, filepath.Join(root, "var/run"), filepath.Join(root, "usr/local/bin"), filepath.Join(root, "Applications"), filepath.Join(contents, "MacOS"), filepath.Join(contents, "Resources/bin/cli-plugins")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, file := range []string{"Info.plist", "MacOS/fabo", "Resources/bin/docker"} {
		write(t, filepath.Join(contents, file), "")
	}
	for _, env := range []string{"FABO_SOCKET", "ZDOTDIR", "BUILDX_CONFIG"} {
		t.Setenv(env, "")
	}
	return &setup{
		in:     installation{dir: filepath.Join(contents, "MacOS"), home: home, names: namesFor(id)},
		docker: filepath.Join(home, ".docker"), root: root, out: io.Discard,
	}
}

func write(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestContextIsWhatDockerContextCreateWrites(t *testing.T) {
	s := sandbox(t, "dev.fabo.app")
	if err := s.writeContext(); err != nil {
		t.Fatal(err)
	}
	// docker names the directory by the SHA-256 of the context's name.
	if got := filepath.Base(s.contextDir("fabo")); got != "cde86679250d261092bc0d50332bd559c0ae95a8fbdc97e06fb4890977bee5c8" {
		t.Fatalf("context directory = %q", got)
	}
	meta, err := readJSON(filepath.Join(s.contextDir("fabo"), "meta.json"))
	if err != nil {
		t.Fatal(err)
	}
	host := meta["Endpoints"].(object)["docker"].(object)["Host"]
	if meta["Name"] != "fabo" || host != "unix://"+filepath.Join(s.in.home, ".fabo/run/docker.sock") {
		t.Fatalf("meta = %v", meta)
	}
}

func TestCommandLinksAreAddedOnceAndTakenBackExactly(t *testing.T) {
	s := sandbox(t, "dev.fabo.app")
	zshrc, config := filepath.Join(s.in.home, ".zshrc"), filepath.Join(s.docker, "config.json")
	write(t, zshrc, "export EDITOR=vim")
	write(t, config, `{"auths":{"ghcr.io":{"auth":"c2VjcmV0"}},"psFormat":"<b>&"}`)
	if err := os.Chmod(config, 0o600); err != nil {
		t.Fatal(err)
	}
	contents := s.contents()
	for range 2 {
		if err := s.bundledCLI(contents); err != nil {
			t.Fatal(err)
		}
	}
	if got := strings.Count(read(t, zshrc), profileMark); got != 1 {
		t.Fatalf(".zshrc has the PATH line %d times", got)
	}
	if !strings.HasPrefix(read(t, zshrc), "export EDITOR=vim\n\n"+profileMark+"\n") {
		t.Fatalf(".zshrc = %q", read(t, zshrc))
	}
	for link, want := range map[string]string{
		filepath.Join(s.binDir(), "fabo"):   filepath.Join(contents, "MacOS/fabo"),
		filepath.Join(s.binDir(), "docker"): filepath.Join(contents, "Resources/bin/docker"),
		s.root + systemLink:                 filepath.Join(s.binDir(), "fabo"),
	} {
		if got, _ := os.Readlink(link); got != want {
			t.Errorf("%s -> %q, want %q", link, got, want)
		}
	}
	cfg, err := readJSON(config)
	if err != nil {
		t.Fatal(err)
	}
	plugins, _ := cfg["cliPluginsExtraDirs"].([]any)
	if cfg["currentContext"] != "fabo" || len(plugins) != 1 || cfg["psFormat"] != "<b>&" {
		t.Fatalf("config = %v", cfg)
	}
	// Credentials stay private, and are not rewritten as HTML escapes.
	if info, _ := os.Stat(config); info.Mode().Perm() != 0o600 {
		t.Errorf("config.json is %v, want 0600", info.Mode().Perm())
	}
	if !strings.Contains(read(t, config), `"<b>&"`) {
		t.Errorf("config.json escaped a value: %s", read(t, config))
	}

	if err := s.removeShellPath(); err != nil {
		t.Fatal(err)
	}
	if got := read(t, zshrc); got != "export EDITOR=vim\n" {
		t.Errorf(".zshrc after uninstall = %q", got)
	}
	if err := s.forgetDocker(); err != nil {
		t.Fatal(err)
	}
	cfg, _ = readJSON(config)
	if _, current := cfg["currentContext"]; current || len(cfg["cliPluginsExtraDirs"].([]any)) != 0 || cfg["auths"] == nil {
		t.Errorf("config after uninstall = %v", cfg)
	}
}

func TestSetupLeavesAnotherEnginesChoicesAlone(t *testing.T) {
	s := sandbox(t, "dev.fabo.app")
	write(t, filepath.Join(s.root, "Applications/OrbStack.app/x"), "")
	write(t, s.root+systemLink, "someone else's fabo")
	if err := s.bundledCLI(s.contents()); err != nil {
		t.Fatal(err)
	}
	if cfg, _ := readJSON(filepath.Join(s.docker, "config.json")); cfg["currentContext"] != nil {
		t.Errorf("docker's default context changed beside OrbStack: %v", cfg)
	}
	if got := read(t, s.root+systemLink); got != "someone else's fabo" {
		t.Errorf("%s was replaced", systemLink)
	}
	if s.otherEngine() != "OrbStack" {
		t.Errorf("otherEngine = %q", s.otherEngine())
	}
}

func TestAFlavoredAppKeepsToItsOwnFiles(t *testing.T) {
	s := sandbox(t, "dev.fabo.app.dev")
	if err := s.bundledCLI(s.contents()); err != nil {
		t.Fatal(err)
	}
	if !isFile(filepath.Join(s.in.home, ".fabo-dev/bin/docker")) && !exists(filepath.Join(s.in.home, ".fabo-dev/bin/fabo")) {
		t.Error("no command links in ~/.fabo-dev/bin")
	}
	for _, untouched := range []string{filepath.Join(s.in.home, ".zshrc"), filepath.Join(s.in.home, ".zprofile"), filepath.Join(s.docker, "config.json"), s.root + systemLink} {
		if _, err := os.Lstat(untouched); err == nil {
			t.Errorf("%s was written", untouched)
		}
	}
}

func TestDockerSockOwner(t *testing.T) {
	s := sandbox(t, "dev.fabo.app")
	link, ours := s.root+dockerSock, s.in.socket()
	orb := filepath.Join(s.in.home, ".orbstack/run/docker.sock")
	owner := func(engines ...string) sockOwner {
		got, _ := dockerSockAt(link, ours, engines)
		return got
	}
	if owner() != sockFree {
		t.Error("nothing there must be free")
	}
	if err := os.Symlink(ours, link); err != nil {
		t.Fatal(err)
	}
	if owner() != sockOurs {
		t.Error("a link to our socket is ours")
	}
	// OrbStack's link with OrbStack not running is still OrbStack's, and
	// free once OrbStack is gone.
	os.Remove(link)
	if err := os.Symlink(orb, link); err != nil {
		t.Fatal(err)
	}
	if owner(filepath.Dir(filepath.Dir(orb))) != sockTaken || owner() != sockFree {
		t.Error("a dangling link belongs to its engine only while that engine is installed")
	}
	// Another engine's own socket.
	os.Remove(link)
	listener, err := net.Listen("unix", link)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if owner() != sockTaken {
		t.Error("a socket of its own is another engine's")
	}
}

func TestTestcontainersFileIsFabosOnlyWhenMarked(t *testing.T) {
	s := sandbox(t, "dev.fabo.app")
	path, ours := filepath.Join(s.in.home, ".testcontainers.properties"), s.in.socket()
	if on, err := testcontainers(path, ours, true); err != nil || !on {
		t.Fatalf("write: %v %v", on, err)
	}
	mine := read(t, path)
	if !strings.Contains(mine, "docker.host=unix://"+ours) {
		t.Fatalf("file = %q", mine)
	}
	// The user's own file, or one Testcontainers has added to, stays.
	for _, text := range []string{"docker.host=tcp://remote:2375\n", mine + "testcontainers.reuse.enable=true\n"} {
		write(t, path, text)
		for _, on := range []bool{true, false} {
			if kept, err := testcontainers(path, ours, on); err != nil || kept || read(t, path) != text {
				t.Errorf("touched a file that is not Fabo's: %q", read(t, path))
			}
		}
	}
	write(t, path, mine)
	if on, err := testcontainers(path, ours, false); err != nil || on || exists(path) {
		t.Errorf("Fabo's own file was not removed: %v %v", on, err)
	}
}

func TestAgentPlistNamesThisLauncher(t *testing.T) {
	s := sandbox(t, "dev.fabo.app.dev")
	s.in.dir = filepath.Join(s.in.home, "A&B <apps>/Fabo Dev.app/Contents/MacOS")
	plist := s.agentPlist()
	for _, want := range []string{
		"<key>Label</key><string>dev.fabo.daemon.dev</string>",
		"<array><string>dev.fabo.app.dev</string></array>",
		"A&amp;B &lt;apps>/Fabo Dev.app/Contents/MacOS/fabo</string><string>daemon</string>",
		filepath.Join(s.in.home, ".fabo-dev/daemon.log"),
	} {
		if !strings.Contains(plist, want) {
			t.Errorf("plist lacks %q", want)
		}
	}
	if s.agentRunsThis() {
		t.Error("no agent is installed yet")
	}
	write(t, s.agentPath(), plist)
	if !s.agentRunsThis() {
		t.Error("the agent just written must be recognised as this app's")
	}
	s.in.dir = filepath.Join(s.in.home, "elsewhere/Fabo Dev.app/Contents/MacOS")
	if s.agentRunsThis() {
		t.Error("an agent for another copy of the app is not this one's")
	}
}

func TestBuildWithFaboIsTakenAndGivenBackExactly(t *testing.T) {
	s := sandbox(t, "dev.fabo.app")
	p := s.places()
	config := `{"aliases":{"builder":"mine"},"auths":{}}`
	write(t, p.config(), config)
	write(t, p.current(), `{"Key":"default","Name":"desktop","Global":false}`)

	// Nothing to take until Fabo's builder exists.
	if err := p.apply("", true); err != nil {
		t.Fatal(err)
	}
	if p.saved() != "fabo" || read(t, p.current()) != `{"Key":"default","Name":"desktop","Global":false}` {
		t.Fatalf("took the builds without a builder: %s", read(t, p.current()))
	}
	write(t, p.instance(p.builder), `{"Name":"fabo-build","Driver":"docker-container","Nodes":[{"Name":"fabo-build0","Endpoint":"fabo"}]}`)
	if err := p.apply("", true); err != nil {
		t.Fatal(err)
	}
	cfg, _ := readJSON(p.config())
	cur, _ := readJSON(p.current())
	if cfg["aliases"].(object)["builder"] != "buildx" || cur["Name"] != "fabo-build" || cur["Global"] != true {
		t.Fatalf("config = %v, current = %v", cfg, cur)
	}
	// The builder named after the context is a copy of Fabo's.
	shadow, _ := readJSON(p.instance("fabo"))
	if shadow["Name"] != "fabo" || !p.copied(shadow) {
		t.Fatalf("shadow = %v", shadow)
	}

	if err := p.choose("docker"); err != nil {
		t.Fatal(err)
	}
	cfg, _ = readJSON(p.config())
	if cfg["aliases"].(object)["builder"] != "mine" || read(t, p.current()) != `{"Key":"default","Name":"desktop","Global":false}` {
		t.Fatalf("not given back: %v, %s", cfg, read(t, p.current()))
	}
	if err := p.uninstall(); err != nil {
		t.Fatal(err)
	}
	if exists(p.instance("fabo")) || exists(p.state) {
		t.Error("uninstall left the copy or build.json")
	}
}

func TestBuildsAreNotTakenUnaskedBesideAnotherEngine(t *testing.T) {
	for name, other := range map[string]string{"another engine": "OrbStack", "a flavored app": ""} {
		s := sandbox(t, "dev.fabo.app")
		p := s.places()
		write(t, p.instance(p.builder), `{"Name":"fabo-build","Nodes":[{"Name":"fabo-build0"}]}`)
		if err := p.apply(other, other != ""); err != nil {
			t.Fatal(err)
		}
		if p.saved() != "" || exists(p.current()) || exists(p.config()) {
			t.Errorf("%s: took the builds unasked", name)
		}
		// A builder of that name that is not Fabo's copy is left alone.
		theirs := `{"Name":"fabo","Nodes":[{"Name":"remote0"}]}`
		write(t, p.instance(p.context), theirs)
		if err := p.apply(other, false); err != nil || read(t, p.instance(p.context)) != theirs {
			t.Errorf("%s: replaced a builder that is not Fabo's copy", name)
		}
	}
}

func TestSetupRefusesAnInstallationThatCannotStart(t *testing.T) {
	s := sandbox(t, "dev.fabo.app")
	var out bytes.Buffer
	s.out = &out
	// No engine in this bundle: nothing may be written, and no agent loaded.
	if err := s.run(nil); err == nil {
		t.Fatal("setup must fail before changing anything")
	}
	for _, untouched := range []string{s.docker, s.agentPath(), s.binDir()} {
		if exists(untouched) {
			t.Errorf("%s was written", untouched)
		}
	}
	if err := s.run([]string{"--frobnicate"}); err == nil || !strings.Contains(err.Error(), "usage") {
		t.Errorf("unknown flag: %v", err)
	}
}

func TestUninstallLeavesAnotherCopysPluginDirectory(t *testing.T) {
	installed := "/Applications/Fabo.app/Contents/Resources/bin/cli-plugins"
	for _, id := range []string{"dev.fabo.app", "dev.fabo.app.dev"} {
		s := sandbox(t, id)
		config := filepath.Join(s.docker, "config.json")
		write(t, config, `{"cliPluginsExtraDirs":["`+installed+`"]}`)
		before := read(t, config)
		if err := s.bundledCLI(s.contents()); err != nil {
			t.Fatal(err)
		}
		if err := s.forgetDocker(); err != nil {
			t.Fatal(err)
		}
		cfg, _ := readJSON(config)
		if dirs, _ := cfg["cliPluginsExtraDirs"].([]any); len(dirs) != 1 || dirs[0] != installed {
			t.Errorf("%s: cliPluginsExtraDirs = %v", id, dirs)
		}
		// A flavored app never wrote the file, so it must not rewrite it.
		if s.in.names.flavored && read(t, config) != before {
			t.Errorf("%s: config.json was rewritten: %s", id, read(t, config))
		}
	}
}

// Upstream Kiri's own kiri command, in this installation's bin directory or
// in /usr/local/bin, is not Fabo's: linking fabo leaves it alone.
func TestFaboLinksLeaveAnExistingKiriCommand(t *testing.T) {
	s := sandbox(t, "dev.fabo.app")
	legacy := filepath.Join(s.binDir(), "kiri")
	write(t, legacy, "existing Kiri command")
	write(t, s.root+"/usr/local/bin/kiri", "existing system Kiri command")
	if err := s.bundledCLI(s.contents()); err != nil {
		t.Fatal(err)
	}
	if target, err := os.Readlink(filepath.Join(s.binDir(), "fabo")); err != nil || target != s.launcher() {
		t.Fatalf("Fabo launcher link = %q, %v", target, err)
	}
	if read(t, legacy) != "existing Kiri command" || read(t, s.root+"/usr/local/bin/kiri") != "existing system Kiri command" {
		t.Fatal("replaced an existing Kiri command")
	}
	s.unlinkSystem()
	if exists(s.root+systemLink) || read(t, legacy) != "existing Kiri command" {
		t.Fatal("Fabo link removal affected Kiri")
	}
}

func TestBuildWithFaboKeepsPersistedBuilderChoice(t *testing.T) {
	s := sandbox(t, "dev.fabo.app.dev")
	p := s.places()
	write(t, p.instance(p.builder), `{"Name":"fabo-dev-build","Nodes":[{"Name":"fabo-dev-build0"}]}`)
	if err := s.buildWith([]string{"fabo"}); err != nil {
		t.Fatal(err)
	}
	if p.saved() != "fabo" {
		t.Fatal("changed the stored builder choice")
	}
	current, err := readJSON(p.current())
	if err != nil || current["Name"] != "fabo-dev-build" {
		t.Fatalf("builder = %v, %v", current, err)
	}
	if err := s.buildWith([]string{"docker"}); err != nil {
		t.Fatal(err)
	}
	if p.saved() != "docker" {
		t.Fatal("could not switch back to Docker")
	}
}
