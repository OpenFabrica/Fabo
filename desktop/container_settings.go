package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/egoist/mygo/ui"
)

type envEntry struct{ name, value string }
type portEntry struct{ ip, host, port, protocol string }
type portBinding struct {
	ContainerPort int    `json:"container_port"`
	HostPort      int    `json:"host_port"`
	HostIP        string `json:"host_ip"`
	Protocol      string `json:"proto"`
}

type containerSettings struct {
	generation uint64
	id         string
	loaded     bool
	problem    string
	env        []envEntry
	ports      []portEntry
}

// Read the configured bindings, including port zero and stopped containers.
// Runtime NetworkSettings describes allocated ports and cannot serve as a draft.
func (e engineClient) containerSettings(ctx context.Context, id string) (containerSettings, error) {
	var info struct {
		Config     struct{ Env []string }
		HostConfig struct {
			PortBindings map[string][]struct{ HostIP, HostPort string }
		}
	}
	draft := containerSettings{id: id}
	if err := e.get(ctx, containerPath(id)+"/json", &info); err != nil {
		return draft, err
	}
	draft.loaded = true
	for _, text := range info.Config.Env {
		k, v, _ := strings.Cut(text, "=")
		draft.env = append(draft.env, envEntry{k, v})
	}
	keys := make([]string, 0, len(info.HostConfig.PortBindings))
	for k := range info.HostConfig.PortBindings {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, key := range keys {
		port, proto, _ := strings.Cut(key, "/")
		for _, b := range info.HostConfig.PortBindings[key] {
			host := b.HostPort
			if host == "" {
				host = "0"
			}
			draft.ports = append(draft.ports, portEntry{b.HostIP, host, port, proto})
		}
	}
	return draft, nil
}

func (d containerSettings) environment() ([]string, error) {
	result := make([]string, 0, len(d.env))
	for i, row := range d.env {
		if row.name == "" || strings.ContainsAny(row.name, "=\x00") || strings.IndexFunc(row.name, unicode.IsSpace) >= 0 || strings.ContainsRune(row.value, 0) {
			return nil, fmt.Errorf(tr("环境变量第 %d 行需要有效名称和值", "Environment row %d needs a valid name and value"), i+1)
		}
		result = append(result, row.name+"="+row.value)
	}
	return result, nil
}

func (d containerSettings) bindings() ([]portBinding, error) {
	result := make([]portBinding, 0, len(d.ports))
	for i, row := range d.ports {
		inner, e1 := strconv.Atoi(strings.TrimSpace(row.port))
		host, e2 := strconv.Atoi(strings.TrimSpace(row.host))
		ip := strings.TrimSpace(row.ip)
		if e1 != nil || e2 != nil || inner < 1 || inner > 65535 || host < 0 || host > 65535 || (row.protocol != "tcp" && row.protocol != "udp") || (ip != "" && net.ParseIP(ip) == nil) {
			return nil, fmt.Errorf(tr("端口第 %d 行无效：容器端口 1–65535，主机端口 0–65535，地址须为 IP", "Invalid port row %d: container 1–65535, host 0–65535, address must be an IP"), i+1)
		}
		result = append(result, portBinding{inner, host, ip, row.protocol})
	}
	return result, nil
}

func (s *desktop) loadContainerSettings(id string) {
	generation := s.containerDraft.generation + 1
	s.containerDraft = containerSettings{id: id, generation: generation}
	s.run(8*time.Second, func(ctx context.Context) func() {
		draft, err := s.engine.containerSettings(ctx, id)
		return func() {
			if s.containerDraft.id != id || s.containerDraft.generation != generation {
				return
			}
			if err != nil {
				draft.problem = err.Error()
			}
			draft.generation = generation
			s.containerDraft = draft
		}
	})
}

func (s *desktop) saveContainerSettings(kind string) {
	d := s.containerDraft
	var body any
	switch kind {
	case "env":
		env, err := d.environment()
		if err != nil {
			s.containerDraft.problem = err.Error()
			return
		}
		body = map[string]any{"env": env}
	case "ports":
		ports, err := d.bindings()
		if err != nil {
			s.containerDraft.problem = err.Error()
			return
		}
		body = map[string]any{"ports": ports}
	default:
		return
	}
	s.containerDraft.problem = ""
	s.working["container-settings"] = true
	s.run(2*time.Minute, func(ctx context.Context) func() {
		err := s.engine.request(ctx, http.MethodPost, "/kiri/containers/"+url.PathEscape(d.id)+"/"+kind, body, nil)
		return func() {
			delete(s.working, "container-settings")
			if s.containerDraft.id == d.id {
				if err != nil {
					s.containerDraft.problem = err.Error()
				} else {
					s.loadContainerSettings(d.id)
				}
			}
			// Even a failed restart can have saved the environment for the next start.
			s.detail.key = ""
			s.settle(err, tr("容器设置已保存", "Container settings saved"))
		}
	})
}

func (s *desktop) containerSettingsTab(c *ui.Context, p palette, x container) {
	if s.containerDraft.id != x.ID {
		s.loadContainerSettings(x.ID)
	}
	d := &s.containerDraft
	if !d.loaded {
		if d.problem != "" {
			note(c, p, d.problem)
			if button(c.Key("retry-settings"), p, tr("重新读取", "Reload"), "", plainButton, false) {
				s.loadContainerSettings(x.ID)
			}
		} else {
			note(c, p, tr("正在读取设置…", "Reading settings…"))
		}
		return
	}
	busy := s.working["container-settings"]
	ui.Scroll(c.Key("container-settings")).Fill().Grow(1).MinHeight(0).Children(func() {
		ui.Column(c).FillWidth().Padding(12, 14).Gap(10).Children(func() {
			if d.problem != "" {
				remark(c, p, d.problem)
				if button(c.Key("retry"), p, tr("重新读取", "Reload"), "", plainButton, busy) {
					s.loadContainerSettings(x.ID)
				}
			}
			heading(c, p, tr("环境变量", "Environment"))
			remark(c, p, tr("保存会重启正在运行的容器，保留文件和挂载。", "Saving restarts a running container and keeps its files and mounts."))
			removeEnv := -1
			for i := range d.env {
				row := &d.env[i]
				ui.Row(c.Key(fmt.Sprintf("env-%d", i))).FillWidth().Gap(6).Children(func() {
					ui.TextInput(c.Key("name"), &row.name).Label(fmt.Sprintf("%s %d", tr("变量名", "Variable name"), i+1)).Placeholder("NAME").Width(180).Height(28).Disabled(busy)
					ui.TextInput(c.Key("value"), &row.value).Label(fmt.Sprintf("%s %d", tr("变量值", "Variable value"), i+1)).Grow(1).Width(0).Height(28).Disabled(busy)
					if iconButton(c.Key("remove"), p, "x", fmt.Sprintf("%s %d", tr("移除变量", "Remove variable"), i+1), busy).Clicked() {
						removeEnv = i
					}
				})
			}
			if removeEnv >= 0 {
				d.env = append(d.env[:removeEnv], d.env[removeEnv+1:]...)
			}
			ui.Row(c).Gap(8).Children(func() {
				if button(c.Key("add-env"), p, tr("添加变量", "Add variable"), "plus", plainButton, busy) {
					d.env = append(d.env, envEntry{})
				}
				if button(c.Key("save-env"), p, tr("保存环境变量", "Save environment"), "", primaryButton, busy || !s.connected) {
					s.pending = &confirmation{tr("保存环境变量？", "Save environment?"), tr("正在运行的容器将重启，文件和挂载会保留。", "A running container will restart. Files and mounts are retained."), tr("保存", "Save"), func() { s.saveContainerSettings("env") }}
				}
			})
			heading(c, p, tr("发布端口", "Published ports"))
			remark(c, p, tr("主机端口 0 自动分配；留空地址监听所有接口。保存立即生效。", "Host port 0 allocates a free port; an empty address listens on all interfaces. Changes apply immediately."))
			removePort := -1
			for i := range d.ports {
				row := &d.ports[i]
				ui.Row(c.Key(fmt.Sprintf("port-%d", i))).FillWidth().Gap(6).Children(func() {
					ui.TextInput(c.Key("ip"), &row.ip).Label(fmt.Sprintf("%s %d", tr("绑定地址", "Bind address"), i+1)).Placeholder("0.0.0.0").Grow(1).Width(0).Height(28).Disabled(busy)
					ui.TextInput(c.Key("host"), &row.host).Label(fmt.Sprintf("%s %d", tr("主机端口", "Host port"), i+1)).Placeholder("8080").Width(95).Height(28).Disabled(busy)
					ui.TextInput(c.Key("port"), &row.port).Label(fmt.Sprintf("%s %d", tr("容器端口", "Container port"), i+1)).Placeholder("80").Width(95).Height(28).Disabled(busy)
					if v, ok := pick(c.Key("proto"), fmt.Sprintf("%s %d", tr("协议", "Protocol"), i+1), row.protocol, []string{"tcp", "udp"}); ok && !busy {
						row.protocol = v
					}
					if iconButton(c.Key("remove"), p, "x", fmt.Sprintf("%s %d", tr("移除端口", "Remove port"), i+1), busy).Clicked() {
						removePort = i
					}
				})
			}
			if removePort >= 0 {
				d.ports = append(d.ports[:removePort], d.ports[removePort+1:]...)
			}
			ui.Row(c).Gap(8).Children(func() {
				if button(c.Key("add-port"), p, tr("添加端口", "Add port"), "plus", plainButton, busy) {
					d.ports = append(d.ports, portEntry{host: "0", protocol: "tcp"})
				}
				if button(c.Key("save-ports"), p, tr("保存端口", "Save ports"), "", primaryButton, busy || !s.connected) {
					s.saveContainerSettings("ports")
				}
			})
		})
	})
}
