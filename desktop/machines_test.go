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
	if !reflect.DeepEqual(commands, [][]string{{s.launcherPath, "machine", "shell", "dev"}}) || s.page != terminalPage {
		t.Fatalf("shell: %q on %s", commands, s.page)
	}

	s.navigate(machinesPage)
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
	if err := tt.ChooseMenuItem("Delete…"); err != nil {
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
	if err := tt.ChooseMenuItem("Debug Shell"); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	if !reflect.DeepEqual(commands, [][]string{{"/usr/local/bin/fabo", "debug", "redis-cache"}}) || s.page != terminalPage {
		t.Fatalf("debug: %q on %s", commands, s.page)
	}
}
