package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/egoist/mygo/ui"
)

const kubernetesPage = "kubernetes"
const kubernetesNode = "fabo-k8s"

type kubernetesConfig struct {
	LAN  bool     `json:"lan"`
	SAN  []string `json:"tls_san"`
	Port int      `json:"port"`
}

type kubeObject struct {
	Kind     string
	Metadata struct {
		Namespace, Name, UID, CreationTimestamp, DeletionTimestamp string
		Labels                                                     map[string]string
		OwnerReferences                                            []struct{ Kind string }
	}
	Spec struct {
		NodeName, Type, ClusterIP string
		Selector                  map[string]string
		Containers                []struct{ Name, Image string }
		InitContainers            []struct{ Name string }
		Ports                     []struct {
			Name, Protocol string
			Port, NodePort int
			TargetPort     any
		}
	}
	Status struct {
		Phase, Reason, PodIP  string
		ContainerStatuses     []kubeContainerStatus
		InitContainerStatuses []kubeContainerStatus
		LoadBalancer          struct {
			Ingress []struct{ IP, Hostname string }
		}
	}
}

type kubeContainerStatus struct {
	Name         string
	Ready        bool
	RestartCount int
	State        struct {
		Waiting    *struct{ Reason, Message string }
		Running    *struct{ StartedAt string }
		Terminated *struct {
			Reason   string
			ExitCode int
		}
	}
}

func (o kubeObject) key() string { return o.Kind + "/" + o.Metadata.Namespace + "/" + o.Metadata.Name }
func (o kubeObject) readiness() string {
	n := 0
	for _, c := range o.Status.ContainerStatuses {
		if c.Ready {
			n++
		}
	}
	return fmt.Sprintf("%d/%d", n, len(o.Spec.Containers))
}
func (o kubeObject) restarts() int {
	n := 0
	for _, c := range o.Status.ContainerStatuses {
		n += c.RestartCount
	}
	return n
}

// Match kubectl's status priorities: deletion, init failures, then main containers.
func (o kubeObject) status() string {
	if o.Metadata.DeletionTimestamp != "" {
		return "Terminating"
	}
	reason := o.Status.Reason
	if reason == "" {
		reason = o.Status.Phase
	}
	if reason == "" {
		reason = "Unknown"
	}
	for i, c := range o.Status.InitContainerStatuses {
		if c.State.Terminated != nil && c.State.Terminated.ExitCode == 0 {
			continue
		}
		if t := c.State.Terminated; t != nil {
			if t.Reason != "" {
				return "Init:" + t.Reason
			}
			return fmt.Sprintf("Init:ExitCode:%d", t.ExitCode)
		}
		if w := c.State.Waiting; w != nil && w.Reason != "" && w.Reason != "PodInitializing" {
			return "Init:" + w.Reason
		}
		return fmt.Sprintf("Init:%d/%d", i, len(o.Spec.InitContainers))
	}
	running := false
	for i := len(o.Status.ContainerStatuses) - 1; i >= 0; i-- {
		c := o.Status.ContainerStatuses[i]
		if w := c.State.Waiting; w != nil && w.Reason != "" {
			reason = w.Reason
		} else if t := c.State.Terminated; t != nil {
			reason = t.Reason
			if reason == "" {
				reason = fmt.Sprintf("ExitCode:%d", t.ExitCode)
			}
		} else if c.State.Running != nil && c.Ready {
			running = true
		}
	}
	if reason == "Completed" && running {
		return "Running"
	}
	return reason
}

func validKubeName(name string) bool {
	if name == "" || len(name) > 253 || !(name[0] >= 'a' && name[0] <= 'z' || name[0] >= '0' && name[0] <= '9') {
		return false
	}
	for _, c := range []byte(name) {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '.') {
			return false
		}
	}
	return true
}

type kubernetesState struct {
	state, problem string
	port           int
	items          []kubeObject
}

type kubernetesUI struct {
	kubernetesState
	loading                bool
	retryAt                time.Time
	tab                    int
	logsKey, logsContainer string
	logsPrevious           bool
	configLoaded           bool
	configProblem          string
	lan                    bool
	sans, apiPort          string
}

func (e engineClient) kubectl(ctx context.Context, background bool, args ...string) (string, error) {
	if background {
		ctx = context.WithValue(ctx, backgroundRead{}, true)
	}
	cmd := append([]string{"timeout", "-s", "KILL", "20", "kubectl"}, args...)
	return e.exec(ctx, kubernetesNode, cmd)
}

func (e engineClient) kubernetes(ctx context.Context) (kubernetesState, error) {
	result := kubernetesState{state: "absent", port: 6443}
	var node struct {
		State struct {
			Status  string
			Running bool
		}
		NetworkSettings struct {
			Ports map[string][]struct{ HostPort string }
		}
	}
	if err := e.get(ctx, containerPath(kubernetesNode)+"/json", &node); err != nil {
		var failed *engineError
		if errors.As(err, &failed) && failed.status == http.StatusNotFound {
			return result, nil
		}
		return result, err
	}
	result.state = node.State.Status
	if ports := node.NetworkSettings.Ports["6443/tcp"]; len(ports) > 0 {
		result.port, _ = strconv.Atoi(ports[0].HostPort)
	}
	if !node.State.Running {
		return result, nil
	}
	out, err := e.kubectl(ctx, true, "get", "pods,services", "-A", "-o", "json", "--request-timeout=15s")
	if err != nil {
		return result, err
	}
	var list struct{ Items []kubeObject }
	if err = json.Unmarshal([]byte(out), &list); err != nil {
		return result, fmt.Errorf("Kubernetes: %w", err)
	}
	result.items = list.Items
	sort.Slice(result.items, func(i, j int) bool { return result.items[i].key() < result.items[j].key() })
	return result, nil
}

func (s *desktop) refreshKubernetes() {
	if s.kube.loading || time.Now().Before(s.kube.retryAt) || s.working["k8s"] {
		return
	}
	s.kube.loading = true
	s.run(25*time.Second, func(ctx context.Context) func() {
		result, err := s.engine.kubernetes(ctx)
		return func() {
			s.kube.loading = false
			if err != nil {
				result.problem = err.Error()
				s.kube.retryAt = time.Now().Add(10 * time.Second)
			}
			s.kube.kubernetesState = result
		}
	})
}

func (s *desktop) kubernetesAction(args ...string) {
	if s.launcher == nil || s.working["k8s"] {
		return
	}
	s.working["k8s"] = true
	s.kube.problem = ""
	s.run(15*time.Minute, func(ctx context.Context) func() {
		_, err := s.launcher(ctx, append([]string{"k8s"}, args...)...)
		return func() {
			delete(s.working, "k8s")
			s.kube.retryAt = time.Time{}
			s.kube.configLoaded = false
			s.settle(err, tr("Kubernetes 操作完成", "Kubernetes operation completed"))
			s.refreshKubernetes()
		}
	})
}

func (s *desktop) kubernetesView(c *ui.Context, p palette) {
	busy := s.working["k8s"]
	toolbar(c, func() {
		ui.Box(c).Grow(1)
		if busy {
			label(c, tr("正在配置 Kubernetes…", "Configuring Kubernetes…"), p.muted)
		}
		if button(c.Key("refresh-k8s"), p, tr("刷新", "Refresh"), "", plainButton, s.kube.loading || busy) {
			s.kube.retryAt = time.Time{}
			s.refreshKubernetes()
		}
		if s.kube.state == "running" {
			if button(c.Key("stop-k8s"), p, tr("停止集群", "Stop cluster"), "stop", plainButton, busy || s.launcher == nil) {
				s.kubernetesAction("stop")
			}
		} else if button(c.Key("start-k8s"), p, tr("启动集群", "Start cluster"), "play", primaryButton, busy || s.launcher == nil) {
			s.kubernetesAction("start")
		}
	})
	if s.kube.problem != "" {
		remark(c, p, s.kube.problem)
	}
	if s.kube.state != "running" {
		text := tr("集群尚未创建或已停止", "The cluster has not been created or is stopped")
		if s.kube.state == "" || s.kube.loading {
			text = tr("正在读取集群…", "Reading cluster…")
		}
		empty(c, p, "helm", text, "")
		return
	}
	cols := []column{{tr("名称", "Name"), 0, 1, false}, {tr("命名空间", "Namespace"), 150, 0, false}, {tr("状态", "Status"), 180, 0, false}, {tr("就绪", "Ready"), 80, 0, false}}
	if s.kube.tab == 1 {
		cols[2].title = tr("类型", "Type")
		cols[3] = column{"Cluster IP", 145, 0, false}
	}
	tableHeader(c, p, cols, 0, nil)
	if !s.dockMax {
		ui.Scroll(c.Key("k8s-items")).FillWidth().Grow(1).MinHeight(0).Children(func() {
			for _, o := range s.kube.items {
				kind := "Pod"
				if s.kube.tab == 1 {
					kind = "Service"
				}
				if o.Kind != kind || !strings.Contains(strings.ToLower(o.Metadata.Namespace+"/"+o.Metadata.Name), strings.ToLower(s.search)) {
					continue
				}
				s.tableRow(c.Key(o.key()), p, o.key(), o.Metadata.Name, 0, nil, func() {
					cell(c, 0, 1).Children(func() { label(c, o.Metadata.Name, p.text) })
					cell(c, 150, 0).Children(func() { label(c, o.Metadata.Namespace, p.muted) })
					cell(c, 180, 0).Children(func() {
						if kind == "Pod" {
							label(c, o.status(), p.text)
						} else {
							label(c, o.Spec.Type, p.text)
						}
					})
					cell(c, cols[3].width, 0).Children(func() {
						if kind == "Pod" {
							label(c, o.readiness(), p.muted)
						} else {
							label(c, o.Spec.ClusterIP, p.muted)
						}
					})
				}, func() {})
			}
		})
	}
	for _, o := range s.kube.items {
		if o.key() == s.selected[kubernetesPage] {
			s.kubernetesDock(c, p, o)
			break
		}
	}
}

func (s *desktop) kubernetesDock(c *ui.Context, p palette, o kubeObject) {
	tabs := []string{tr("信息", "Info")}
	if o.Kind == "Pod" {
		tabs = append(tabs, tr("日志", "Logs"))
	}
	s.dock(c, p, o.Metadata.Name, nil, tabs, func() {
		if o.Kind == "Pod" && iconButton(c.Key("delete-pod"), p, "trash", tr("删除 Pod", "Delete pod"), s.working["pod-delete"]).Clicked() {
			s.pending = &confirmation{tr("删除 Pod？", "Delete pod?"), o.Metadata.Namespace + "/" + o.Metadata.Name + tr("；控制器可能自动重建它。", "; its controller may recreate it."), tr("删除", "Delete"), func() { s.deletePod(o) }}
		}
	}, func(tab int) {
		if tab == 0 {
			fields := []field{{tr("名称", "Name"), o.Metadata.Name}, {tr("命名空间", "Namespace"), o.Metadata.Namespace}, {tr("创建时间", "Created"), o.Metadata.CreationTimestamp}}
			if o.Kind == "Pod" {
				fields = append(fields, field{tr("状态", "Status"), o.status()}, field{"IP", o.Status.PodIP}, field{"Node", o.Spec.NodeName}, field{tr("重启次数", "Restarts"), strconv.Itoa(o.restarts())})
				for _, v := range o.Spec.Containers {
					fields = append(fields, field{v.Name, v.Image})
				}
			} else {
				fields = append(fields, field{tr("类型", "Type"), o.Spec.Type}, field{"Cluster IP", o.Spec.ClusterIP})
				for _, v := range o.Spec.Ports {
					fields = append(fields, field{v.Name, fmt.Sprintf("%d → %v/%s", v.Port, v.TargetPort, v.Protocol)})
				}
				for _, v := range o.Status.LoadBalancer.Ingress {
					fields = append(fields, field{"Ingress", v.IP + v.Hostname})
				}
			}
			keyValues(c, p, fields)
			return
		}
		s.podLogsTab(c, p, o)
	})
}

func (s *desktop) deletePod(o kubeObject) {
	if !validKubeName(o.Metadata.Namespace) || !validKubeName(o.Metadata.Name) {
		s.notices = append(s.notices, tr("Pod 名称无效", "Invalid pod name"))
		return
	}
	s.working["pod-delete"] = true
	s.run(25*time.Second, func(ctx context.Context) func() {
		_, err := s.engine.kubectl(ctx, false, "delete", "pod", "-n", o.Metadata.Namespace, o.Metadata.Name, "--wait=false")
		return func() {
			delete(s.working, "pod-delete")
			s.settle(err, tr("Pod 删除请求已发送", "Pod deletion requested"))
			s.refreshKubernetes()
		}
	})
}

func (s *desktop) podLogsTab(c *ui.Context, p palette, o kubeObject) {
	var names []string
	for _, v := range o.Spec.Containers {
		names = append(names, v.Name)
	}
	if len(names) == 0 {
		note(c, p, tr("没有容器", "No containers"))
		return
	}
	if s.kube.logsKey != o.key()+"/"+o.Metadata.UID {
		s.kube.logsKey = o.key() + "/" + o.Metadata.UID
		s.kube.logsContainer = names[0]
		s.kube.logsPrevious = false
	}
	toolbar(c, func() {
		if v, ok := pick(c.Key("container"), tr("容器", "Container"), s.kube.logsContainer, names); ok {
			s.kube.logsContainer = v
			if s.podLogs != nil {
				s.retireLogs(s.podLogs)
				s.podLogs = nil
			}
		}
		previous := s.kube.logsPrevious
		ui.Checkbox(c.Key("previous"), &s.kube.logsPrevious, tr("上一次运行", "Previous run"))
		if previous != s.kube.logsPrevious {
			if s.podLogs != nil {
				s.retireLogs(s.podLogs)
				s.podLogs = nil
			}
		}
		if button(c.Key("refresh-logs"), p, tr("刷新日志", "Refresh logs"), "", plainButton, false) {
			if s.podLogs != nil {
				s.retireLogs(s.podLogs)
				s.podLogs = nil
			}
		}
	})
	if s.podLogs != nil && s.podLogs.key == podLogKey(o, s.kube.logsContainer, s.kube.logsPrevious) {
		source := s.podLogs.snapshot()
		if source.problem != "" {
			remark(c, p, source.problem)
		}
		if len(source.lines) > 0 {
			s.logWell(c, "pod-logs", source.lines, numbered(source.lines, source.first), true, true)
		}
	}
}

func (s *desktop) loadKubernetesConfig() {
	s.kube.configLoaded = true
	cfg := kubernetesConfig{}
	path := s.prefsFile
	if path == "" {
		path = prefsPath()
	}
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(path), "k8s.json"))
	if err == nil {
		err = json.Unmarshal(raw, &cfg)
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		s.kube.configProblem = err.Error()
	}
	if cfg.Port == 0 {
		cfg.Port = 6443
	}
	s.kube.lan, s.kube.sans, s.kube.apiPort = cfg.LAN, strings.Join(cfg.SAN, ","), strconv.Itoa(cfg.Port)
}

func (s *desktop) kubernetesPane(c *ui.Context, p palette) {
	if !s.kube.configLoaded {
		s.loadKubernetesConfig()
	}
	busy := s.working["k8s"]
	heading(c, p, "Kubernetes")
	pref(c, p, tr("集群", "Cluster"), tr("启动本地 k3s 并写入 kubectl context。", "Run local k3s and write its kubectl context."), func() {
		if busy {
			value(c, p, tr("操作进行中…", "Working…"))
			return
		}
		action, title := "start", tr("启动", "Start")
		if s.kube.state == "running" {
			action, title = "stop", tr("停止", "Stop")
		}
		if button(c, p, title, "", primaryButton, s.launcher == nil) {
			s.kubernetesAction(action)
		}
	})
	pref(c, p, "API", fmt.Sprintf("https://127.0.0.1:%d", s.kube.port), func() {
		if button(c, p, tr("复制地址", "Copy address"), "copy", plainButton, s.kube.state != "running") {
			c.WriteClipboard(fmt.Sprintf("https://127.0.0.1:%d", s.kube.port))
		}
	})
	pref(c, p, tr("局域网访问", "LAN access"), tr("允许局域网设备访问 Kubernetes API。", "Allow devices on the LAN to reach the Kubernetes API."), func() {
		ui.Checkbox(c, &s.kube.lan, tr("允许局域网访问", "Allow LAN access")).Disabled(busy || s.launcher == nil)
	})
	input(c, p, "k8s-port", tr("API 端口", "API port"), "6443", &s.kube.apiPort, false).Disabled(busy || s.launcher == nil)
	input(c, p, "k8s-san", tr("证书额外域名或 IP", "Additional certificate names or IPs"), "example.local,192.168.1.10", &s.kube.sans, false).Disabled(busy || s.launcher == nil)
	remark(c, p, tr("保存网络设置会重建节点，保留集群数据。", "Saving network settings recreates the node and keeps cluster data."))
	if s.kube.configProblem != "" {
		remark(c, p, s.kube.configProblem)
	}
	if s.kube.problem != "" {
		remark(c, p, s.kube.problem)
	}
	if button(c.Key("save-k8s"), p, tr("保存网络设置", "Save network settings"), "", primaryButton, busy || s.launcher == nil) {
		port, err := strconv.Atoi(s.kube.apiPort)
		if err != nil || port < 1024 || port > 65535 {
			s.kube.configProblem = tr("端口须为 1024–65535", "Port must be 1024–65535")
			return
		}
		lan := "off"
		if s.kube.lan {
			lan = "on"
		}
		sans := s.kube.sans
		s.pending = &confirmation{tr("保存 Kubernetes 网络设置？", "Save Kubernetes network settings?"), tr("节点将重建，集群数据会保留。", "The node will be recreated. Cluster data is retained."), tr("保存", "Save"), func() { s.kubernetesAction("set", "--lan", lan, "--tls-san", sans, "--port", strconv.Itoa(port)) }}
	}
	pref(c, p, tr("删除集群", "Delete cluster"), tr("删除节点和所有集群数据，无法撤销。", "Delete the node and all cluster data permanently."), func() {
		if button(c, p, tr("删除集群…", "Delete cluster…"), "trash", dangerButton, busy || s.launcher == nil) {
			s.pending = &confirmation{tr("删除 Kubernetes 集群？", "Delete Kubernetes cluster?"), tr("所有 Kubernetes 工作负载和集群数据将被删除。", "All Kubernetes workloads and cluster data will be deleted."), tr("删除", "Delete"), func() { s.kubernetesAction("delete") }}
		}
	})
	if s.launcher == nil {
		remark(c, p, tr("请从 Fabo 应用打开，以管理集群。", "Open from the Fabo app to manage the cluster."))
	}
}
