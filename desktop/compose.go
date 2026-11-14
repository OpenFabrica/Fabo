package main

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/egoist/mygo/ui"
)

// composeEditor is the Compose file open in a project's Config tab. key
// ties the text to the project and file it was read from, so a read that
// lands after the selection moved is dropped.
type composeEditor struct {
	key, path    string
	text, saved  string
	loaded, busy bool
	problem      string
	fileAt       int
}

// composeProject gathers what a project's members say about the project.
type composeProject struct {
	name                               string
	members                            []container
	running                            int
	services, networks, volumes, ports []string
	workDir, envFile, version          string
	files                              []string
	created                            int64
}

// readProject collects a project's facts from its members' labels and
// settings, each value once, in the members' order.
func readProject(name string, members []container) composeProject {
	pr := composeProject{name: name, members: members}
	seen := map[string]bool{}
	add := func(list *[]string, kind, v string) {
		if v != "" && !seen[kind+v] {
			seen[kind+v] = true
			*list = append(*list, v)
		}
	}
	configs := ""
	for _, x := range members {
		if x.bucket() == "running" {
			pr.running++
		}
		add(&pr.services, "s", x.Service)
		for _, n := range x.Networks {
			add(&pr.networks, "n", n.Network)
		}
		for _, m := range x.Mounts {
			// An anonymous volume's name is a 64-digit ID; its start is enough.
			name := m.Name
			if len(name) == 64 && strings.Trim(name, "0123456789abcdef") == "" {
				name = name[:12]
			}
			add(&pr.volumes, "v", name)
		}
		for _, pt := range x.Ports {
			add(&pr.ports, "p", fmt.Sprintf("%d→%d", pt.Public, pt.Private))
		}
		if pr.created == 0 || (x.Created > 0 && x.Created < pr.created) {
			pr.created = x.Created
		}
		pr.workDir = cmp.Or(pr.workDir, x.WorkDir)
		pr.envFile = cmp.Or(pr.envFile, x.EnvFile)
		pr.version = cmp.Or(pr.version, x.ComposeVersion)
		configs = cmp.Or(configs, x.ConfigFiles)
	}
	for _, f := range strings.Split(configs, ",") {
		if f = strings.TrimSpace(f); f != "" {
			pr.files = append(pr.files, f)
		}
	}
	return pr
}

// projectDock shows a Compose project: where and how it was brought up,
// what its members share, its Compose file to edit, and the members
// themselves. Its buttons act on all of them.
func (s *desktop) projectDock(c *ui.Context, p palette, name string, members []container) {
	pr := readProject(name, members)
	join := func(list []string) string { return strings.Join(list, ", ") }
	tabs := []string{tr("信息", "Info"), tr("配置", "Config"), fmt.Sprintf(tr("容器 %d", "Containers %d"), len(members))}
	s.dock(c, p, name, func() {
		ui.Text(c, "Compose").FontSize(11).TextColor(p.sub).Padding(1, 7).Radius(10).Background(p.soft).Shrink(0)
	}, tabs, func() {
		each := func(running bool, act func(container)) {
			for _, x := range members {
				if (x.bucket() == "running") == running {
					act(x)
				}
			}
		}
		if pr.running < len(members) && iconButton(c.Key("start-all"), p, "play", tr("全部启动 ", "Start all in ")+name, false).Clicked() {
			each(false, s.startContainer)
		}
		if pr.running > 0 && iconButton(c.Key("restart-all"), p, "restart", tr("全部重启 ", "Restart all in ")+name, false).Clicked() {
			each(true, s.restartContainer)
		}
		if pr.running > 0 && iconButton(c.Key("stop-all"), p, "stop", tr("全部停止 ", "Stop all in ")+name, false).Clicked() {
			each(true, s.stopContainer)
		}
		if pr.workDir != "" && iconButton(c.Key("open-dir"), p, "folder", tr("打开项目目录", "Open the project folder"), false).Clicked() {
			c.OpenURL((&url.URL{Scheme: "file", Path: pr.workDir}).String())
		}
		if s.launcher != nil && iconButton(c.Key("pack-project"), p, "archive", tr("打包项目", "Pack the project"), s.working["pack"]).Clicked() {
			s.pack(nil, name, name)
		}
		if iconButton(c.Key("delete-all"), p, "trash", tr("删除项目的全部容器", "Delete the project's containers"), false).Clicked() {
			s.removeContainers(members, fmt.Sprintf(tr("删除 %s 的 %d 个容器？", "Delete the %[2]d containers of %[1]s?"), name, len(members)))
		}
	}, func(tab int) {
		switch tab {
		case 1:
			s.composeConfigTab(c, p, pr)
		case 2:
			s.users(c, p, members)
		default:
			keyValues(c, p, compact([]field{
				{tr("状态", "State"), fmt.Sprintf(tr("%d/%d 运行中", "%d/%d running"), pr.running, len(members))},
				{tr("服务", "Compose services"), join(pr.services)},
				{tr("Compose 版本", "Compose version"), pr.version},
				{tr("工作目录", "Directory"), pr.workDir},
				{tr("配置文件", "Config files"), strings.Join(pr.files, "\n")},
				{tr("环境文件", "Environment file"), pr.envFile},
				{tr("网络", "Networks"), join(pr.networks)},
				{tr("卷", "Volumes"), join(pr.volumes)},
				{tr("端口", "Ports"), join(pr.ports)},
				{tr("创建", "Created"), strings.TrimPrefix(since(s.now, pr.created), "—")},
			}))
		}
	})
}

// composeConfigTab edits the project's Compose file where it lives. Save
// writes it; Save and apply also runs docker compose up -d, so the
// containers follow what the file now says.
func (s *desktop) composeConfigTab(c *ui.Context, p palette, pr composeProject) {
	if len(pr.files) == 0 {
		note(c, p, tr("这个项目没有记录 Compose 文件。", "This project has no recorded Compose file."))
		return
	}
	at := min(max(s.compose.fileAt, 0), len(pr.files)-1)
	if key := pr.name + "\x00" + pr.files[at]; s.compose.key != key {
		s.loadCompose(key, pr.files[at], at)
	}
	e := &s.compose
	ui.Row(c.Key("compose-file")).FillWidth().Shrink(0).Padding(12, 16, 8).Gap(8).AlignItems(ui.Center).Children(func() {
		if len(pr.files) > 1 {
			names := make([]string, len(pr.files))
			for i, f := range pr.files {
				names[i] = filepath.Base(f)
			}
			if chosen, ok := pick(c.Key("compose-pick"), tr("Compose 文件", "Compose file"), names[at], names); ok {
				for i, n := range names {
					if n == chosen {
						s.compose.fileAt = i
					}
				}
			}
		}
		label(c, e.path, p.muted).Grow(1)
	})
	if e.problem != "" {
		ui.Text(c, e.problem).FontSize(12).LineHeight(1.45).TextColor(p.err).Padding(0, 16, 8)
	}
	if !e.loaded {
		if e.problem == "" {
			note(c, p, tr("正在读取 Compose 文件…", "Reading the Compose file…"))
		}
		return
	}
	ui.TextArea(c.Key("compose-text"), &e.text).Label(tr("Compose 文件", "Compose file")).Font(mono).FontSize(12).
		Grow(1).MinHeight(120).Margin(0, 16).Radius(10)
	changed := e.text != e.saved
	ui.Row(c.Key("compose-actions")).FillWidth().Shrink(0).Padding(10, 16, 16).Gap(8).AlignItems(ui.Center).Children(func() {
		if e.busy {
			label(c, tr("正在应用…", "Applying…"), p.muted)
		}
		ui.Box(c).Grow(1)
		if button(c.Key("compose-revert"), p, tr("还原", "Revert"), "", ghostButton, !changed || e.busy) {
			e.text = e.saved
		}
		if button(c.Key("compose-save"), p, tr("保存", "Save"), "", plainButton, !changed || e.busy) {
			s.saveCompose(pr, false)
		}
		if button(c.Key("compose-apply"), p, tr("保存并应用", "Save and apply"), "", primaryButton, e.busy) {
			s.saveCompose(pr, true)
		}
	})
}

// loadCompose reads one of a project's Compose files off the main thread.
func (s *desktop) loadCompose(key, path string, at int) {
	s.compose = composeEditor{key: key, path: path, fileAt: at}
	s.run(5*time.Second, func(context.Context) func() {
		data, err := os.ReadFile(path)
		return func() {
			if s.compose.key != key {
				return
			}
			if err != nil {
				// The path is on screen already; say only what failed.
				var pathErr *fs.PathError
				if errors.As(err, &pathErr) {
					err = pathErr.Err
				}
				s.compose.problem = fmt.Sprintf(tr("读不到 %s：%v", "Could not read %s: %v"), filepath.Base(path), err)
				return
			}
			s.compose.text, s.compose.saved, s.compose.loaded = string(data), string(data), true
		}
	})
}

// saveCompose writes the edited file, keeping its permissions, and with
// apply brings the project up again from all of its files.
func (s *desktop) saveCompose(pr composeProject, apply bool) {
	e := &s.compose
	key, path, text := e.key, e.path, e.text
	e.busy, e.problem = true, ""
	s.run(10*time.Minute, func(ctx context.Context) func() {
		mode := os.FileMode(0o644)
		if info, err := os.Stat(path); err == nil {
			mode = info.Mode().Perm()
		}
		err := os.WriteFile(path, []byte(text), mode)
		if err == nil && apply {
			err = composeUp(ctx, pr)
		}
		return func() {
			if s.compose.key == key {
				s.compose.busy = false
				if err == nil {
					s.compose.saved = text
				} else {
					s.compose.problem = err.Error()
				}
			}
			done := tr("已保存 ", "Saved ") + filepath.Base(path)
			if apply {
				done = tr("已应用 ", "Applied ") + pr.name
			}
			s.settle(err, done)
		}
	})
}

// composeUp runs Compose's up -d for a project from its directory against
// the window's own engine, named by DOCKER_HOST so that no docker context
// or plugin setting of the user's is needed.
func composeUp(ctx context.Context, pr composeProject) error {
	args := []string{"-p", pr.name}
	for _, f := range pr.files {
		args = append(args, "-f", f)
	}
	args = append(args, "up", "-d")
	tool, err := composeCLI()
	if err != nil {
		return err
	}
	if filepath.Base(tool) == "docker" || filepath.Base(tool) == "docker.exe" {
		args = append([]string{"compose"}, args...)
	}
	cmd := exec.CommandContext(ctx, tool, args...)
	cmd.Dir = pr.workDir
	cmd.Env = append(os.Environ(), "DOCKER_HOST="+engineHost())
	out, err := cmd.CombinedOutput()
	if err != nil {
		// The last lines of Compose's output say what went wrong.
		lines := strings.Split(strings.TrimSpace(string(out)), "\n")
		reason := strings.Join(lines[max(0, len(lines)-3):], "\n")
		return fmt.Errorf("docker compose up: %s", cmp.Or(reason, err.Error()))
	}
	return nil
}

// composeCLI finds a program that runs Compose: the Compose plugin itself,
// which also runs on its own, from this installation, the user's docker
// configuration or the usual plugin places; else docker, for its compose
// command. An app started from the Finder has a short PATH, so the usual
// install places are tried after it.
func composeCLI() (string, error) {
	home, _ := os.UserHomeDir()
	exe, _ := os.Executable()
	config := os.Getenv("DOCKER_CONFIG")
	if config == "" {
		config = filepath.Join(home, ".docker")
	}
	dirs := []string{filepath.Join(filepath.Dir(exe), "../Resources/bin/cli-plugins"), filepath.Join(config, "cli-plugins")}
	if data, err := os.ReadFile(filepath.Join(config, "config.json")); err == nil {
		var settings struct{ CliPluginsExtraDirs []string }
		if json.Unmarshal(data, &settings) == nil {
			dirs = append(dirs, settings.CliPluginsExtraDirs...)
		}
	}
	dirs = append(dirs, "/usr/local/lib/docker/cli-plugins", "/opt/homebrew/lib/docker/cli-plugins", "/usr/libexec/docker/cli-plugins",
		"/usr/lib/docker/cli-plugins", "/Applications/Docker.app/Contents/Resources/cli-plugins")
	for _, dir := range dirs {
		if path, err := exec.LookPath(filepath.Join(dir, "docker-compose")); err == nil {
			return path, nil
		}
	}
	if path, err := exec.LookPath("docker-compose"); err == nil {
		return path, nil
	}
	for _, dir := range []string{"", "/usr/local/bin", "/opt/homebrew/bin", filepath.Join(home, ".fabo/bin"), filepath.Join(home, ".docker/bin"),
		filepath.Join(filepath.Dir(exe), "../Resources/bin"), "/Applications/Docker.app/Contents/Resources/bin"} {
		if path, err := exec.LookPath(filepath.Join(dir, "docker")); err == nil {
			return path, nil
		}
	}
	return "", errors.New(tr("找不到 docker 命令，无法应用 Compose 文件", "The docker command was not found, so the Compose file cannot be applied"))
}
