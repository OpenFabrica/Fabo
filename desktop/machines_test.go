package main

import (
	"context"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/egoist/mygo/plugins/terminal"
	"github.com/egoist/mygo/ui"
)

// Machines have a page of their own and stay out of the containers list,
// as do Fabo's helpers; a machine's shell opens in the window's terminal
// through the launcher, and New machine asks the launcher to make one.
func TestMachinesPage(t *testing.T) {
	f, s := newFakeEngine(t)
	var commands [][]string
	s.launcherPath = "/Applications/Fabo.app/Contents/MacOS/fabo"
	s.openTerminal = func(o terminal.Options) { commands = append(commands, o.Command) }
	var made [][]string
	s.launcher = func(_ context.Context, args ...string) ([]byte, error) {
		if args[0] == "machine" {
			made = append(made, slices.Clone(args))
		}
		return nil, nil
	}
	tt := ui.NewTester(s.view, 1180, 760)
	for _, hidden := range []string{"machine-dev", "fabo-debug-c-web-1"} {
		if tt.HasText(hidden) {
			t.Fatalf("%s is in the containers list", hidden)
		}
	}
	click(t, tt, "Machines")
	if m := has(tt, "dev", "Ubuntu", "Running", "old", "Alpine", "Stopped", "1 running"); m != "" {
		t.Fatalf("missing %q in %q", m, tt.Texts())
	}
	// A stopped machine has no shell to open; it can be started.
	_ = tt.Click("Open a shell in old")
	if len(commands) != 0 {
		t.Fatal("opened a shell in a stopped machine")
	}
	click(t, tt, "Start old")
	if got := strings.Join(f.changed(), " "); !strings.Contains(got, "/containers/m-old/start") {
		t.Fatalf("start: %s", got)
	}
	click(t, tt, "Open a shell in dev")
	if !reflect.DeepEqual(commands, [][]string{{s.launcherPath, "machine", "shell", "dev"}}) || s.page != machinesPage || s.machineTerm != "dev" {
		t.Fatalf("shell: %q on %s/%s", commands, s.page, s.machineTerm)
	}
	tt.Frame()
	click(t, tt, "Back to machines")
	if s.machineTerm != "" || len(s.machineTabs["dev"]) != 1 {
		t.Fatalf("back: %q, tabs %v", s.machineTerm, s.machineTabs)
	}
	// Coming back shows the shells still open rather than a new one.
	click(t, tt, "Open a shell in dev")
	if len(commands) != 1 || s.machineTerm != "dev" {
		t.Fatalf("reopened: %q", commands)
	}
	s.machineTerm = ""
	tt.Frame()
	click(t, tt, "New machine")
	s.form.name, s.form.image, s.form.flag = "work", "Debian", true
	tt.Frame()
	click(t, tt, "Create")
	if !reflect.DeepEqual(made, [][]string{{"machine", "create", "work", "--distro", "debian", "--systemd"}}) {
		t.Fatalf("create: %q", made)
	}
	// A taken or malformed name keeps the dialog open with the reason.
	for _, name := range []string{"dev", "-x", "a b"} {
		s.form = form{kind: "machine", image: "Ubuntu", name: name}
		tt.Frame()
		click(t, tt, "Create")
		if s.form.kind != "machine" || s.form.problem == "" {
			t.Fatalf("name %q accepted", name)
		}
	}
	s.form = form{}
	tt.Frame()
	click(t, tt, "More actions for dev")
	if err := tt.Click("Delete…"); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	click(t, tt, "Delete")
	if got := strings.Join(f.changed(), " "); !strings.Contains(got, "DELETE /containers/m-dev?force=1&v=1") {
		t.Fatalf("delete: %s", got)
	}
}

// A running container's menu opens a debug shell beside it in the
// window's terminal.
func TestDebugShellOpensFromTheContainerMenu(t *testing.T) {
	_, s := newFakeEngine(t)
	var commands [][]string
	s.launcherPath = "/usr/local/bin/fabo"
	s.openTerminal = func(o terminal.Options) { commands = append(commands, o.Command) }
	tt := ui.NewTester(s.view, 1180, 760)
	click(t, tt, "More actions for redis-cache")
	if err := tt.Click("Debug Shell"); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	if !reflect.DeepEqual(commands, [][]string{{"/usr/local/bin/fabo", "debug", "redis-cache"}}) || s.page != terminalPage {
		t.Fatalf("debug: %q on %s", commands, s.page)
	}
}

// A machine's terminal page belongs to it: New terminal opens another of
// its shells as a tab, closing a tab ends that shell, and closing the last
// one, or the shell ending, closes the page.
func TestMachineTerminalTabs(t *testing.T) {
	_, s := newFakeEngine(t)
	var commands [][]string
	s.launcherPath = "/x/fabo"
	s.openTerminal = func(o terminal.Options) {
		commands = append(commands, o.Command)
		if o.Theme == nil || o.Theme.Background != ui.Hex("#000000") {
			t.Errorf("terminal background %v, want black", o.Theme)
		}
	}
	s.navigate(machinesPage)
	tt := ui.NewTester(s.view, 1180, 760)
	click(t, tt, "Open a shell in dev")
	tt.Frame()
	if m := has(tt, "dev", "Ubuntu", "Shell 1"); m != "" {
		t.Fatalf("missing %q in %q", m, tt.Texts())
	}
	click(t, tt, "New terminal")
	tt.Frame()
	if len(commands) != 2 || !tt.HasText("Shell 2") || s.machineActive["dev"] != s.machineTabs["dev"][1] {
		t.Fatalf("second shell: %q, tabs %v", commands, s.machineTabs)
	}
	click(t, tt, "Shell 1")
	if s.machineActive["dev"] != s.machineTabs["dev"][0] {
		t.Fatal("picking a tab did not show it")
	}
	click(t, tt, "Close Shell 1")
	tt.Frame()
	if len(s.machineTabs["dev"]) != 1 || s.machineTerm != "dev" || tt.HasText("Shell 1") {
		t.Fatalf("after closing one: %v", s.machineTabs)
	}
	// A new shell takes the next number, not one still in use.
	click(t, tt, "New terminal")
	tt.Frame()
	if !tt.HasText("Shell 3") {
		t.Fatalf("numbering: %q", tt.Texts())
	}
	click(t, tt, "Close Shell 2")
	click(t, tt, "Close Shell 3")
	tt.Frame()
	if s.machineTerm != "" || len(s.machineTabs["dev"]) != 0 || !tt.HasText("old") {
		t.Fatalf("the page stayed after its last shell: %q", s.machineTerm)
	}

	// A shell that exits closes its tab; the last one closes the page.
	click(t, tt, "Open a shell in dev")
	key := s.machineTabs["dev"][0]
	session := s.terminals[key]
	session.mu.Lock()
	session.exited = true
	session.mu.Unlock()
	tt.Frame()
	tt.Frame()
	if s.machineTerm != "" || s.terminals[key] != nil {
		t.Fatal("an ended shell kept its page")
	}
}

// The terminal background is black by default; Settings changes it for new
// terminals and those already open.
func TestTerminalBackgroundSetting(t *testing.T) {
	_, s := newFakeEngine(t)
	if light, dark := s.termThemes(); light.Background != ui.Hex("#000000") || dark != light {
		t.Fatalf("default: %v %v", light.Background, dark.Background)
	}
	s.prefs.TermBg = "white"
	if light, _ := s.termThemes(); light.Background != ui.Hex("#FFFFFF") {
		t.Fatalf("white: %v", light.Background)
	}
	s.prefs.TermBg = "system"
	if light, dark := s.termThemes(); light.Background != ui.Hex("#FFFFFF") || dark.Background != ui.Hex("#000000") {
		t.Fatalf("system: %v %v", light.Background, dark.Background)
	}
	s.prefs.TermBg = ""
	s.navigate(settingsPage)
	tt := ui.NewTester(s.view, 1180, 900)
	click(t, tt, "Graphite")
	if s.prefs.TermBg != "graphite" || s.termBackground() != ui.Hex("#1E1F22") {
		t.Fatalf("graphite: %q %v", s.prefs.TermBg, s.termBackground())
	}
}
