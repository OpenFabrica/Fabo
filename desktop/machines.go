package main

// Linux machines: long-lived distro containers that `fabo machine` makes,
// with the user's account and the Mac's home folders inside. They have a
// page of their own and stay out of the containers list. Their shells, and
// the debug shell of a container, open in the window's terminal.

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/egoist/mygo/plugins/terminal"
	"github.com/egoist/mygo/ui"
)

const machinesPage = "machines"

// Labels that set a container apart: a Linux machine, and the short-lived
// helpers Fabo itself runs (debug sessions, volume copies for packages and
// imports).
const (
	machineLabel = "dev.fabo.machine"
	debugLabel   = "dev.fabo.debug"
)

var helperLabels = []string{debugLabel, "dev.fabo.pack-helper", "dev.fabo.import-helper"}

// machineDistros are the distributions `fabo machine create --distro`
// knows, in the order the dialog offers them.
var machineDistros = []struct{ id, name, image string }{
	{"ubuntu", "Ubuntu", "ubuntu:24.04"}, {"debian", "Debian", "debian:trixie"}, {"alpine", "Alpine", "alpine:3.22"},
	{"fedora", "Fedora", "fedora:42"}, {"arch", "Arch Linux", "archlinux:latest"}, {"rocky", "Rocky Linux", "rockylinux:9"},
	{"opensuse", "openSUSE", "opensuse/tumbleweed:latest"},
}

// distroName names a machine's distribution by its image, or the image.
func distroName(image string) string {
	for _, d := range machineDistros {
		if image == d.image {
			return d.name
		}
	}
	return image
}

// splitContainers keeps the containers list to the user's containers: the
// machines go to their own list and Fabo's helpers to neither.
func splitContainers(all []container) (containers, machines []container) {
	for _, x := range all {
		switch {
		case x.Machine != "":
			machines = append(machines, x)
		case !x.Helper:
			containers = append(containers, x)
		}
	}
	return containers, machines
}

// machine finds a machine by its container ID.
func (s *desktop) machine(id string) (container, bool) {
	for _, x := range s.machines {
		if x.ID == id {
			return x, true
		}
	}
	return container{}, false
}

// machinesView is the machines page: the list with each machine's state
// and shell, and New machine.
func (s *desktop) machinesView(c *ui.Context, p palette) {
	if s.machineTerm != "" && s.machineTerminalView(c, p) {
		return
	}
	query := strings.ToLower(strings.TrimSpace(s.search))
	var shown []container
	for _, x := range s.machines {
		if strings.Contains(strings.ToLower(x.Machine+" "+x.Image), query) {
			shown = append(shown, x)
		}
	}
	toolbar(c, func() {
		ui.Text(c, fmt.Sprintf(tr("%d 台机器", "%d machines"), len(s.machines))).FontSize(12).FontFeatures("tnum").TextColor(p.muted)
		ui.Box(c).Grow(1)
		creating := s.working["machine-create"]
		label := tr("新建机器", "New machine")
		if creating {
			label = tr("创建中…", "Creating…")
		}
		if button(c.Key("new-machine"), p, label, "plus", primaryButton, creating || s.launcher == nil) {
			s.form = form{kind: "machine", image: machineDistros[0].name}
		}
	})
	cols := []column{{tr("名称", "Name"), 0, 1, false}, {tr("发行版", "Distribution"), 130, 0, false},
		{tr("状态", "State"), 120, 0, false}, {tr("创建", "Created"), 86, 0, false}}
	const actionsWidth = 92
	if !s.dockMax {
		tableHeader(c, p, narrowColumns(cols, s.narrow(), 3), actionsWidth, nil)
		if len(shown) == 0 {
			title := tr("没有匹配的机器", "No matching machines")
			if len(s.machines) == 0 {
				title = tr("还没有 Linux 机器", "No Linux machines yet")
			}
			if empty(c, p, "cpu", title, tr("新建机器", "New machine")) && s.launcher != nil {
				s.form = form{kind: "machine", image: machineDistros[0].name}
			}
		} else {
			narrow := s.narrow()
			ui.List(c.Key("rows"), s.list(machinesPage), len(shown)).FillWidth().Grow(1).MinHeight(0).
				ItemKey(func(i int) any { return shown[i].ID }).
				Rows(func(r ui.ListRow) {
					rc, x := r.Context, shown[r.Index]
					running := x.bucket() == "running"
					s.tableRow(rc, p, x.ID, x.Machine, actionsWidth, nil, func() {
						cell(rc, 0, cols[0].grow).Gap(10).Children(func() {
							imageTile(rc, p, x.Image, "cpu")
							label(rc, x.Machine, p.text).FontWeight(500)
						})
						cell(rc, cols[1].width, 0).Children(func() { label(rc, distroName(x.Image), p.text) })
						cell(rc, cols[2].width, 0).Gap(7).Children(func() {
							kind, word := machineState(x)
							dot(rc, p, kind)
							label(rc, word, p.text)
						})
						if !narrow {
							cell(rc, cols[3].width, 0).Children(func() { label(rc, since(s.now, x.Created), p.muted) })
						}
					}, func() {
						s.machineActions(rc, p, x, running)
					})
				})
		}
	}
	if x, ok := s.machine(s.selected[machinesPage]); ok {
		s.machineDock(c, p, x)
	}
}

// machineState is a machine's state as a dot and a word.
func machineState(x container) (kind, word string) {
	switch x.bucket() {
	case "running":
		return "running", tr("运行中", "Running")
	case "failed":
		return "failed", tr("异常", "Failed")
	}
	return "stopped", tr("已停止", "Stopped")
}

// machineActions are a machine row's buttons: its shell, start or stop,
// and the rest in a menu.
func (s *desktop) machineActions(c *ui.Context, p palette, x container, running bool) {
	busy := s.working[x.ID]
	if iconButton(c.Key("shell"), p, "terminal", tr("打开终端 ", "Open a shell in ")+x.Machine, !running || s.launcherPath == "").Clicked() {
		s.openMachineShell(x)
	}
	if running {
		if iconButton(c.Key("stop"), p, "stop", tr("停止 ", "Stop ")+x.Machine, busy).Clicked() {
			s.stopContainer(x)
		}
	} else if iconButton(c.Key("start"), p, "play", tr("启动 ", "Start ")+x.Machine, busy).Clicked() {
		s.startContainer(x)
	}
	var entries []menuEntry
	if running {
		entries = append(entries, menuEntry{label: tr("重启", "Restart"), run: func() { s.restartContainer(x) }})
	}
	entries = append(entries,
		menuEntry{label: tr("复制终端命令", "Copy the shell command"), run: func() {
			c.WriteClipboard("fabo machine shell " + x.Machine)
			s.notices = append(s.notices, tr("已复制", "Copied"))
		}},
		menuEntry{},
		menuEntry{label: tr("删除…", "Delete…"), danger: true, run: func() { s.removeMachine(x) }})
	s.moreMenu(c.Key("more"), p, tr("更多操作 ", "More actions for ")+x.Machine, entries)
}

// machineDock shows a machine: what it runs and how to reach it.
func (s *desktop) machineDock(c *ui.Context, p palette, x container) {
	_, word := machineState(x)
	s.dock(c, p, x.Machine, func() { label(c, word, p.muted).Shrink(0) }, []string{tr("信息", "Info")}, func() {
		if iconButton(c.Key("dock-shell"), p, "terminal", tr("打开终端 ", "Open a shell in ")+x.Machine, x.bucket() != "running" || s.launcherPath == "").Clicked() {
			s.openMachineShell(x)
		}
		if iconButton(c.Key("dock-delete"), p, "trash", tr("删除 ", "Delete ")+x.Machine, false).Clicked() {
			s.removeMachine(x)
		}
	}, func(int) {
		keyValues(c, p, compact([]field{
			{tr("发行版", "Distribution"), distroName(x.Image)},
			{tr("镜像", "Image"), x.Image},
			{tr("状态", "State"), word},
			{tr("用户", "User"), os.Getenv("USER")},
			{tr("共享目录", "Shared folder"), machineHomes()},
			{tr("终端命令", "Shell command"), "fabo machine shell " + x.Machine},
			{tr("容器", "Container"), x.Name},
			{tr("创建", "Created"), strings.TrimPrefix(since(s.now, x.Created), "—")},
		}))
	})
}

// machineHomes is the host folder every machine shares at the same path.
func machineHomes() string {
	if runtime.GOOS == "darwin" {
		return "/Users"
	}
	return "/home"
}

// removeMachine asks before deleting a machine. Its own files go; those
// under the shared home folders stay on the host.
func (s *desktop) removeMachine(x container) {
	s.pending = &confirmation{
		title: fmt.Sprintf(tr("删除机器 %s？", "Delete the machine %s?"), x.Machine),
		body: fmt.Sprintf(tr("机器里安装的软件和其他文件会被删除；%s 下的文件在宿主机上，不受影响。", "What was installed in it goes; files under %s are on the host and stay."),
			machineHomes()),
		ok: tr("删除", "Delete"),
		run: func() {
			s.do(x.ID, engineAction{http.MethodDelete, containerPath(x.ID) + "?force=1&v=1", nil}, fmt.Sprintf(tr("已删除机器 %s", "Deleted the machine %s"), x.Machine))
		},
	}
}

// machineForm is the New machine dialog's fields.
func (s *desktop) machineForm(c *ui.Context, p palette) (submitted bool) {
	f := &s.form
	submitted = input(c, p, "name", tr("名称", "Name"), "work", &f.name, true).Submitted()
	names := make([]string, len(machineDistros))
	for i, d := range machineDistros {
		names[i] = d.name
	}
	ui.Row(c).FillWidth().Gap(10).AlignItems(ui.Center).Children(func() {
		ui.Text(c, tr("发行版", "Distribution")).FontSize(12).TextColor(p.sub).Grow(1)
		if chosen, ok := pick(c.Key("distro"), tr("发行版", "Distribution"), f.image, names); ok {
			f.image = chosen
		}
	})
	ui.Checkbox(c.Key("systemd"), &f.flag, tr("运行 systemd（可用 systemctl 管理服务）", "Run systemd (manage services with systemctl)"))
	return submitted
}

// createMachine checks the dialog and asks the launcher to make the
// machine: it pulls the distribution, adds the user and starts it.
func (s *desktop) createMachine() bool {
	f := &s.form
	name := f.name
	if !validMachineName(name) {
		f.problem = tr("名称只能用字母、数字和连字符，且不能以连字符开头", "Use letters, digits and dashes, not starting with a dash")
		return false
	}
	for _, x := range s.machines {
		if x.Machine == name {
			f.problem = tr("已有同名机器", "A machine with this name exists")
			return false
		}
	}
	args := []string{"machine", "create", name}
	for _, d := range machineDistros {
		if d.name == f.image {
			args = append(args, "--distro", d.id)
		}
	}
	if f.flag {
		args = append(args, "--systemd")
	}
	s.working["machine-create"] = true
	s.page = machinesPage
	s.run(15*time.Minute, func(ctx context.Context) func() {
		_, err := s.launcher(ctx, args...)
		return func() {
			delete(s.working, "machine-create")
			s.settle(err, fmt.Sprintf(tr("机器 %s 已运行", "The machine %s is running"), name))
		}
	})
	if s.engine.http == nil {
		delete(s.working, "machine-create")
	}
	return true
}

// validMachineName matches what `fabo machine` accepts.
func validMachineName(name string) bool {
	if name == "" || name[0] == '-' {
		return false
	}
	for _, b := range []byte(name) {
		if !(b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' || b == '-') {
			return false
		}
	}
	return true
}

// openMachineShell shows the machine's own terminal page, opening its first
// shell when it has none yet.
func (s *desktop) openMachineShell(x container) {
	if s.launcherPath == "" {
		return
	}
	if s.page != machinesPage {
		s.navigate(machinesPage)
	}
	s.machineTerm = x.Machine
	if len(s.machineTabs[x.Machine]) == 0 {
		s.newMachineTerminal(x.Machine)
	}
}

// newMachineTerminal opens another shell in a machine, as a tab of its
// terminal page.
func (s *desktop) newMachineTerminal(name string) {
	s.hostSequence++
	key := fmt.Sprintf("machine/%s/%d", name, s.hostSequence)
	number := 1
	for _, k := range s.machineTabs[name] {
		if t := s.terminals[k]; t != nil {
			var n int
			if _, err := fmt.Sscanf(t.label, tr("终端 %d", "Shell %d"), &n); err == nil && n >= number {
				number = n + 1
			}
		}
	}
	s.terminals[key] = s.launcherSession(fmt.Sprintf(tr("终端 %d", "Shell %d"), number), "machine", "shell", name)
	s.machineTabs[name] = append(s.machineTabs[name], key)
	s.machineActive[name] = key
}

// closeMachineTerminal ends one of a machine's shells; the page closes
// with its last one.
func (s *desktop) closeMachineTerminal(name, key string) {
	s.retireTerminal(key)
	tabs := slices.DeleteFunc(s.machineTabs[name], func(k string) bool { return k == key })
	s.machineTabs[name] = tabs
	if s.machineActive[name] == key {
		s.machineActive[name] = ""
		if len(tabs) > 0 {
			s.machineActive[name] = tabs[len(tabs)-1]
		}
	}
	if len(tabs) == 0 && s.machineTerm == name {
		s.machineTerm = ""
	}
}

// machineTerminalView is a machine's terminal page: a tab per shell, New
// terminal, and the chosen shell. It reports false when there is nothing to
// show (the machine is gone or stopped, or its last shell ended), and the
// list shows again.
func (s *desktop) machineTerminalView(c *ui.Context, p palette) bool {
	name := s.machineTerm
	var x container
	found := false
	for _, m := range s.machines {
		if m.Machine == name {
			x, found = m, true
		}
	}
	// A shell that exited, as after typing exit, closes its tab.
	for _, key := range slices.Clone(s.machineTabs[name]) {
		t := s.terminals[key]
		if t == nil {
			s.closeMachineTerminal(name, key)
			continue
		}
		if _, exited, _, _ := t.snapshot(); exited || !found || x.bucket() != "running" {
			s.closeMachineTerminal(name, key)
		}
	}
	if s.machineTerm == "" || len(s.machineTabs[name]) == 0 {
		s.machineTerm = ""
		return false
	}
	toolbar(c, func() {
		// Closing a tab changes the list, so the row walks a copy.
		for _, key := range slices.Clone(s.machineTabs[name]) {
			t := s.terminals[key]
			if t == nil {
				continue
			}
			on := key == s.machineActive[name]
			chip := ui.Row(c.Key("tab-"+key)).Height(30).Shrink(0).Padding(0, 4, 0, 4).Gap(2).Radius(8).AlignItems(ui.Center)
			if on {
				chip.Background(p.soft)
			}
			chip.Children(func() {
				b := clickable(c.Key("pick")).Label(t.label).Height(26).PaddingX(8).Radius(6).Center()
				ink := p.sub
				if on {
					ink = p.text
				} else if b.Hovered() {
					b.Background(p.soft)
				}
				b.Children(func() {
					ui.Icon(c, icons["terminal"]).Size(13, 13).TextColor(ink)
					ui.Text(c, t.label).FontSize(12.5).FontWeight(500).TextColor(ink).Padding(0, 0, 0, 6)
				})
				if b.Clicked() {
					s.machineActive[name] = key
				}
				if iconButton(c.Key("close"), p, "x", tr("关闭 ", "Close ")+t.label, false).Size(22, 22).Clicked() {
					s.closeMachineTerminal(name, key)
				}
			})
		}
		if iconButton(c.Key("new-machine-terminal"), p, "plus", tr("新建终端", "New terminal"), false).Clicked() {
			s.newMachineTerminal(name)
		}
		ui.Box(c).Grow(1)
	})
	// The last tab closed while this frame drew the toolbar: the list shows
	// from the next frame, which this asks for.
	if s.machineTerm == "" {
		c.AnimationFrame()
		return true
	}
	key := s.machineActive[name]
	session := s.terminals[key]
	if session == nil {
		return true
	}
	term, _, problem, _ := session.snapshot()
	if problem != "" {
		remark(c, p, problem)
	}
	ui.Column(c.Key("machine-well")).Grow(1).MinHeight(0).Margin(0, pageInset, 16).Radius(10).Clip().Background(s.termBackground()).Children(func() {
		if term != nil {
			terminal.View(c.Key(key), term).Fill().AutoFocus().Label(fmt.Sprintf(tr("%s 的终端", "Terminal of %s"), name))
		}
	})
	return true
}

// openDebugShell opens a terminal tab with a debug shell beside x.
func (s *desktop) openDebugShell(x container) {
	s.openLauncherTerminal(tr("调试 ", "debug ")+x.Name, "debug", x.Name)
}

// launcherSession starts one of this installation's fabo commands in a
// terminal with the chosen background.
func (s *desktop) launcherSession(label string, args ...string) *terminalSession {
	home, _ := os.UserHomeDir()
	light, dark := s.termThemes()
	opts := terminal.Options{Command: append([]string{s.launcherPath}, args...), Dir: home, Env: terminalEnvironment(s.prefsFile),
		Font: terminal.Font{Family: s.prefs.Mono, Size: s.prefs.LogSize}, Scrollback: 4 << 20,
		Theme: light, DarkTheme: dark, Padding: terminal.Padding{X: 14, Y: 12}}
	if s.openTerminal != nil {
		// Tests see the command instead of starting it; the session stays
		// open until closed.
		s.openTerminal(opts)
		return newTerminalSession(s.ctx, label, "", 0, s.invalidate, func(ctx context.Context) (*terminal.Terminal, *execStream, error) {
			<-ctx.Done()
			return nil, nil, ctx.Err()
		})
	}
	return newTerminalSession(s.ctx, label, "", 0, s.invalidate, func(ctx context.Context) (*terminal.Terminal, *execStream, error) {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		term, err := terminal.New(opts)
		return term, nil, err
	})
}

// openLauncherTerminal runs one of this installation's fabo commands in a
// new tab of the window's terminal page and shows it.
func (s *desktop) openLauncherTerminal(label string, args ...string) {
	if s.launcherPath == "" {
		return
	}
	s.hostSequence++
	key := "host/" + fmt.Sprint(s.hostSequence)
	s.terminals[key] = s.launcherSession(label, args...)
	s.hostSessions = append(s.hostSessions, key)
	s.hostActive = key
	s.navigate(terminalPage)
}

// launcherBesideExecutable is where findLauncher looks first: FABO_LAUNCHER,
// else the fabo beside this executable, as in the app bundle.
func launcherBesideExecutable() string {
	if path := os.Getenv("FABO_LAUNCHER"); path != "" {
		return path
	}
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	return filepath.Join(filepath.Dir(exe), "fabo")
}

// hasLabel reports whether labels carry any of names.
func hasLabel(labels map[string]string, names []string) bool {
	return slices.ContainsFunc(names, func(n string) bool { _, ok := labels[n]; return ok })
}
