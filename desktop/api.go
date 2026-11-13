package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type portMap struct {
	Private, Public int
	Type            string
}

type mount struct{ Type, Name, Source, Destination string }

type attachment struct{ Network, IP string }

// container is one row of the containers page, from /containers/json.
type container struct {
	ID, Name, Image, ImageID, State, Project, Command string
	Created, Started, Finished                        int64
	ExitCode                                          int
	Ports                                             []portMap
	Mounts                                            []mount
	Networks                                          []attachment
}

// bucket sorts a container into the filter it belongs to.
func (c container) bucket() string {
	switch c.State {
	case "running", "paused", "restarting":
		return "running"
	case "exited", "dead":
		if c.ExitCode != 0 {
			return "failed"
		}
	}
	return "stopped"
}

// status is the sentence of the status column, in the window's language.
func (c container) status(now int64) string {
	switch c.State {
	case "running":
		if c.Started <= 0 {
			return tr("运行中", "Running")
		}
		return tr("已运行 ", "Up ") + ago(now-c.Started)
	case "paused":
		return tr("已暂停", "Paused")
	case "restarting":
		return tr("重启中…", "Restarting…")
	case "created":
		return tr("已创建", "Created")
	}
	text := fmt.Sprintf(tr("退出 (%d)", "Exited (%d)"), c.ExitCode)
	if c.Finished > 0 {
		text += " · " + since(now, c.Finished)
	}
	return text
}

type image struct {
	ID      string
	Tags    []string
	Size    int64
	Created int64
}

// ref is the name to show and to address the image by.
func (i image) ref() string {
	if len(i.Tags) > 0 {
		return i.Tags[0]
	}
	return shortID(i.ID)
}

type volume struct {
	Name, Driver, Mountpoint string
	Created                  int64
}

type network struct {
	ID, Name, Driver, Subnet, Gateway string
	Created                           int64
}

// builtin reports the three networks every engine has and none can remove.
func (n network) builtin() bool { return n.Name == "bridge" || n.Name == "host" || n.Name == "none" }

// vmResources is /kiri/resources. An engine running directly on Linux
// reports zero CPUs.
type vmResources struct {
	CPUs          int  `json:"cpus"`
	Memory        int  `json:"memory_mib"`
	RunningCPUs   int  `json:"running_cpus"`
	RunningMemory int  `json:"running_memory_mib"`
	Awake         bool `json:"awake"`
	MaxCPUs       int  `json:"max_cpus"`
	MaxMemory     int  `json:"max_memory_mib"`
}

type builderInfo struct {
	CacheGB        float64 `json:"cache_gb"`
	DefaultCacheGB float64 `json:"default_cache_gb"`
	CacheLimit     int64   `json:"cache_limit"`
	IdleSecs       int     `json:"idle_secs"`
	Image          string  `json:"image"`
	Container      string  `json:"container"`
	State          string  `json:"state"`
}

type domainPort struct {
	Port    int  `json:"port"`
	Default int  `json:"default"`
	Running bool `json:"running"`
	Serving bool `json:"serving"`
	// Env names the environment variable that fixes this port, if one does.
	Env *string `json:"env"`
}

type domainInfo struct {
	HTTP  domainPort `json:"http"`
	HTTPS domainPort `json:"https"`
}

// proxySettings is what /kiri/proxy stores: where image pulls and pushes
// find their proxy, and whether new containers are told about it.
type proxySettings struct {
	Mode       string `json:"mode"` // system, custom, none
	HTTP       string `json:"http"`
	HTTPS      string `json:"https"`
	NoProxy    string `json:"no_proxy"`
	Containers bool   `json:"containers"`
}

// proxyInfo is the answer of /kiri/proxy: the settings and the proxy they
// resolve to right now.
type proxyInfo struct {
	Settings  proxySettings `json:"settings"`
	Effective struct {
		HTTP    string   `json:"http"`
		HTTPS   string   `json:"https"`
		NoProxy []string `json:"no_proxy"`
		PAC     bool     `json:"pac"`
	} `json:"effective"`
}

// nameState is what the launcher's `kiri domains` prints: whether
// *.kiri.local resolves to the engine, the suffix links carry, and whether
// the engine's certificate authority is trusted for HTTPS.
type nameState struct {
	On     bool   `json:"on"`
	Suffix string `json:"suffix"`
	HTTPS  bool   `json:"https"`
}

type versionInfo struct{ Version, ApiVersion, Os, Arch, GoVersion string }

// diskUsage sums /system/df; volumes holds each volume's size by name.
type diskUsage struct {
	images, containers, volumeTotal, buildCache int64
	volumes                                     map[string]int64
}

type engineEvent struct {
	Kind, Action, Name string
	Time               int64
}

type layer struct {
	By    string `json:"by"`
	Size  int64  `json:"size"`
	Empty bool   `json:"empty"`
}

type finding struct {
	ID       string   `json:"id"`
	Severity string   `json:"severity"`
	Package  string   `json:"package"`
	Version  string   `json:"version"`
	Fixed    []string `json:"fixed"`
}

// scanResult is a finished image scan; counts tallies findings by severity.
type scanResult struct {
	Scanned  int64     `json:"scanned"`
	Packages int       `json:"packages"`
	Vulns    []finding `json:"vulns"`
}

var severities = []string{"Critical", "High", "Medium", "Low", "Negligible", "Unknown"}

// severityRank orders findings from the worst down; unknown names go last.
func severityRank(s string) int {
	for i, name := range severities {
		if strings.EqualFold(name, s) {
			return i
		}
	}
	return len(severities)
}

// counts tallies the findings of a scan by severity rank.
func (r scanResult) counts() [6]int {
	var n [6]int
	for _, v := range r.Vulns {
		n[min(severityRank(v.Severity), 5)]++
	}
	return n
}

// sample is one reading of a container's counters; usage is what the window
// derives from two of them.
type sample struct {
	total, system uint64
	cpus          int
	memory        uint64
}

type usage struct {
	last   sample
	cpu    float64 // percent of one core
	memory uint64
	hist   []float32
}

func shortID(id string) string {
	id = strings.TrimPrefix(id, "sha256:")
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// get is a background read: it never extends the life of an idle VM.
func (e engineClient) get(ctx context.Context, path string, out any) error {
	return e.request(ctx, http.MethodGet, path, nil, out)
}

func (e engineClient) containers(ctx context.Context) ([]container, error) {
	var items []struct {
		ID, Image, ImageID, Command, State string
		Names                              []string
		Created                            int64
		KiriStartedAt, KiriFinishedAt      *int64
		KiriExitCode                       *int
		Labels                             map[string]string
		Ports                              []struct {
			Type                    string
			PrivatePort, PublicPort int
		}
		NetworkSettings struct {
			Networks map[string]struct{ IPAddress string }
		}
		Mounts []mount
	}
	if err := e.get(ctx, "/containers/json?all=1", &items); err != nil {
		return nil, err
	}
	rows := make([]container, 0, len(items))
	for _, item := range items {
		c := container{ID: item.ID, Name: item.ID, Image: item.Image, ImageID: item.ImageID, State: item.State,
			Command: item.Command, Created: item.Created, Project: item.Labels["com.docker.compose.project"], Mounts: item.Mounts}
		if len(item.Names) > 0 {
			c.Name = strings.TrimPrefix(item.Names[0], "/")
		}
		if item.KiriStartedAt != nil {
			c.Started = *item.KiriStartedAt
		}
		if item.KiriFinishedAt != nil {
			c.Finished = *item.KiriFinishedAt
		}
		if item.KiriExitCode != nil {
			c.ExitCode = *item.KiriExitCode
		}
		// A port published on IPv4 and IPv6 is listed twice.
		seen := map[[2]int]bool{}
		for _, p := range item.Ports {
			if key := [2]int{p.PublicPort, p.PrivatePort}; p.PublicPort > 0 && !seen[key] {
				seen[key] = true
				c.Ports = append(c.Ports, portMap{p.PrivatePort, p.PublicPort, p.Type})
			}
		}
		sort.Slice(c.Ports, func(i, j int) bool { return c.Ports[i].Public < c.Ports[j].Public })
		for name, n := range item.NetworkSettings.Networks {
			c.Networks = append(c.Networks, attachment{name, n.IPAddress})
		}
		sort.Slice(c.Networks, func(i, j int) bool { return c.Networks[i].Network < c.Networks[j].Network })
		rows = append(rows, c)
	}
	// Compose members stay together, in name order; everything else keeps
	// the engine's order, newest first.
	sort.SliceStable(rows, func(i, j int) bool {
		return rows[i].Project != "" && rows[i].Project == rows[j].Project && rows[i].Name < rows[j].Name
	})
	return rows, nil
}

func (e engineClient) images(ctx context.Context) ([]image, error) {
	var items []struct {
		ID            string
		RepoTags      []string
		Size, Created int64
	}
	if err := e.get(ctx, "/images/json", &items); err != nil {
		return nil, err
	}
	rows := make([]image, 0, len(items))
	for _, item := range items {
		rows = append(rows, image{ID: item.ID, Tags: item.RepoTags, Size: item.Size, Created: item.Created})
	}
	return rows, nil
}

func (e engineClient) volumes(ctx context.Context) ([]volume, error) {
	var result struct {
		Volumes []struct{ Name, Driver, Mountpoint, CreatedAt string }
	}
	if err := e.get(ctx, "/volumes", &result); err != nil {
		return nil, err
	}
	rows := make([]volume, 0, len(result.Volumes))
	for _, item := range result.Volumes {
		v := volume{Name: item.Name, Driver: item.Driver, Mountpoint: item.Mountpoint}
		if at, err := time.Parse(time.RFC3339, item.CreatedAt); err == nil {
			v.Created = at.Unix()
		}
		rows = append(rows, v)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Name < rows[j].Name })
	return rows, nil
}

func (e engineClient) networks(ctx context.Context) ([]network, error) {
	var items []struct {
		ID, Name, Driver, Created string
		IPAM                      struct {
			Config []struct{ Subnet, Gateway string }
		}
	}
	if err := e.get(ctx, "/networks", &items); err != nil {
		return nil, err
	}
	rows := make([]network, 0, len(items))
	for _, item := range items {
		n := network{ID: item.ID, Name: item.Name, Driver: item.Driver}
		if len(item.IPAM.Config) > 0 {
			n.Subnet, n.Gateway = item.IPAM.Config[0].Subnet, item.IPAM.Config[0].Gateway
		}
		if at, err := time.Parse(time.RFC3339Nano, item.Created); err == nil {
			n.Created = at.Unix()
		}
		rows = append(rows, n)
	}
	// The built-in networks lead, then the rest by name.
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].builtin() != rows[j].builtin() {
			return rows[i].builtin()
		}
		return rows[i].Name < rows[j].Name
	})
	return rows, nil
}

// samples reads the counters of each running container once. A few requests
// run at a time; a container that fails to answer is left out.
func (e engineClient) samples(ctx context.Context, ids []string) map[string]sample {
	out := make(map[string]sample, len(ids))
	var mu sync.Mutex
	var wg sync.WaitGroup
	slots := make(chan struct{}, 6)
	for _, id := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			slots <- struct{}{}
			defer func() { <-slots }()
			var raw struct {
				CPU struct {
					Usage struct {
						Total uint64 `json:"total_usage"`
					} `json:"cpu_usage"`
					System uint64 `json:"system_cpu_usage"`
					Online int    `json:"online_cpus"`
				} `json:"cpu_stats"`
				Memory struct {
					Usage uint64 `json:"usage"`
					Stats struct {
						Inactive uint64 `json:"inactive_file"`
					} `json:"stats"`
				} `json:"memory_stats"`
			}
			if e.get(ctx, "/containers/"+url.PathEscape(id)+"/stats?stream=false&one-shot=1", &raw) != nil {
				return
			}
			s := sample{total: raw.CPU.Usage.Total, system: raw.CPU.System, cpus: raw.CPU.Online, memory: raw.Memory.Usage}
			// Page cache the kernel can drop is not the container's own use.
			if raw.Memory.Stats.Inactive < s.memory {
				s.memory -= raw.Memory.Stats.Inactive
			}
			mu.Lock()
			out[id] = s
			mu.Unlock()
		}()
	}
	wg.Wait()
	return out
}

// advance folds a new sample into a container's usage: CPU is the share of
// the system's time the container took between the two readings.
func (u *usage) advance(s sample) {
	if u.last.system > 0 && s.system > u.last.system && s.total >= u.last.total {
		u.cpu = float64(s.total-u.last.total) / float64(s.system-u.last.system) * float64(max(1, s.cpus)) * 100
		u.hist = append(u.hist, float32(u.cpu))
		if len(u.hist) > 24 {
			u.hist = u.hist[len(u.hist)-24:]
		}
	}
	u.last, u.memory = s, s.memory
}

func (e engineClient) diskUsage(ctx context.Context) (*diskUsage, error) {
	var raw struct {
		LayersSize int64
		Containers []struct{ SizeRw int64 }
		Volumes    []struct {
			Name      string
			UsageData struct{ Size int64 }
		}
		BuildCache []struct{ Size int64 }
	}
	if err := e.get(ctx, "/system/df", &raw); err != nil {
		return nil, err
	}
	d := &diskUsage{images: raw.LayersSize, volumes: map[string]int64{}}
	for _, c := range raw.Containers {
		d.containers += c.SizeRw
	}
	for _, v := range raw.Volumes {
		d.volumes[v.Name] = v.UsageData.Size
		d.volumeTotal += max(0, v.UsageData.Size)
	}
	for _, b := range raw.BuildCache {
		d.buildCache += b.Size
	}
	return d, nil
}

// events returns the last hour of container and image events, newest first.
// The engine closes the stream at until, so this read ends by itself.
func (e engineClient) events(ctx context.Context, now int64) ([]engineEvent, error) {
	var raw []byte
	path := fmt.Sprintf("/events?since=%d&until=%d", now-3600, now)
	if err := e.get(ctx, path, &raw); err != nil {
		return nil, err
	}
	var out []engineEvent
	for _, line := range strings.Split(string(raw), "\n") {
		var ev struct {
			Type, Action string
			Actor        struct{ Attributes map[string]string }
			Time         int64 `json:"time"`
		}
		if json.Unmarshal([]byte(line), &ev) != nil || (ev.Type != "container" && ev.Type != "image") {
			continue
		}
		// Exec and health events would drown the ones worth reading.
		if strings.ContainsRune(ev.Action, ':') || ev.Action == "create" || ev.Action == "kill" {
			continue
		}
		out = append(out, engineEvent{Kind: ev.Type, Action: ev.Action, Name: ev.Actor.Attributes["name"], Time: ev.Time})
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

func (e engineClient) layers(ctx context.Context, ref string) ([]layer, error) {
	var raw struct {
		Images []struct {
			Layers []layer `json:"layers"`
		} `json:"images"`
	}
	if err := e.get(ctx, "/kiri/images/layers?name="+url.QueryEscape(ref), &raw); err != nil {
		return nil, err
	}
	if len(raw.Images) == 0 {
		return nil, nil
	}
	return raw.Images[0].Layers, nil
}

// lastScan returns the newest stored scan of an image, or nil when it was
// never scanned.
func (e engineClient) lastScan(ctx context.Context, ref string) (*scanResult, error) {
	var result *scanResult
	err := e.get(ctx, "/kiri/images/scan?name="+url.QueryEscape(ref), &result)
	return result, err
}

// scan runs a new scan, reporting progress between 0 and 1 as it goes.
func (e engineClient) scan(ctx context.Context, ref string, progress func(float64)) (*scanResult, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://localhost/kiri/images/scan?name="+url.QueryEscape(ref), nil)
	if err != nil {
		return nil, err
	}
	res, err := e.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, responseError(res)
	}
	lines := bufio.NewReader(res.Body)
	for {
		line, err := lines.ReadBytes('\n')
		var step struct {
			Pct    float64     `json:"pct"`
			Result *scanResult `json:"result"`
			Error  string      `json:"error"`
		}
		if json.Unmarshal(line, &step) == nil {
			switch {
			case step.Error != "":
				return nil, fmt.Errorf("%s", step.Error)
			case step.Result != nil:
				return step.Result, nil
			}
			progress(step.Pct / 100)
		}
		if err != nil {
			return nil, fmt.Errorf("the scan ended without a result")
		}
	}
}

// pull fetches an image. The engine reports only the start and the end.
func (e engineClient) pull(ctx context.Context, ref string) error {
	name, tag := ref, "latest"
	if i := strings.LastIndex(ref, ":"); i > strings.LastIndex(ref, "/") {
		name, tag = ref[:i], ref[i+1:]
	}
	var raw []byte
	path := "/images/create?fromImage=" + url.QueryEscape(name) + "&tag=" + url.QueryEscape(tag)
	if err := e.request(ctx, http.MethodPost, path, nil, &raw); err != nil {
		return err
	}
	for _, line := range strings.Split(string(raw), "\n") {
		var step struct{ Error string }
		if json.Unmarshal([]byte(line), &step) == nil && step.Error != "" {
			return fmt.Errorf("%s", step.Error)
		}
	}
	return nil
}

// runSpec is what the run dialog collects.
type runSpec struct {
	Image, Name, Command    string
	HostPort, ContainerPort int
}

// run creates a container from the spec and starts it.
func (e engineClient) run(ctx context.Context, spec runSpec) error {
	body := map[string]any{"Image": spec.Image}
	if fields := strings.Fields(spec.Command); len(fields) > 0 {
		body["Cmd"] = fields
	}
	if spec.HostPort > 0 && spec.ContainerPort > 0 {
		key := strconv.Itoa(spec.ContainerPort) + "/tcp"
		body["ExposedPorts"] = map[string]any{key: struct{}{}}
		body["HostConfig"] = map[string]any{"PortBindings": map[string]any{key: []map[string]string{{"HostPort": strconv.Itoa(spec.HostPort)}}}}
	}
	path := "/containers/create"
	if spec.Name != "" {
		path += "?name=" + url.QueryEscape(spec.Name)
	}
	var created struct{ ID string }
	if err := e.request(ctx, http.MethodPost, path, body, &created); err != nil {
		return err
	}
	return e.request(ctx, http.MethodPost, "/containers/"+created.ID+"/start", nil, nil)
}

// exec runs one command in a container and returns what it printed. The
// engine always answers an attached exec by upgrading the connection, so the
// output is read from the upgraded stream until the command ends.
func (e engineClient) exec(ctx context.Context, id string, cmd []string) (string, error) {
	var created struct{ ID string }
	spec := map[string]any{"Cmd": cmd, "AttachStdout": true, "AttachStderr": true}
	if err := e.request(ctx, http.MethodPost, "/containers/"+url.PathEscape(id)+"/exec", spec, &created); err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://localhost/exec/"+created.ID+"/start", strings.NewReader(`{"Detach":false}`))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "tcp")
	res, err := e.http.Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusSwitchingProtocols && res.StatusCode != http.StatusOK {
		return "", responseError(res)
	}
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 256<<10))
	return logText(raw, false), nil
}

// logs returns the tail of a container's output as lines.
func (e engineClient) logs(ctx context.Context, id string) ([]string, error) {
	path := "/containers/" + url.PathEscape(id)
	var details struct{ Config struct{ Tty bool } }
	if err := e.get(ctx, path+"/json", &details); err != nil {
		return nil, err
	}
	var raw []byte
	if err := e.get(ctx, path+"/logs?stdout=1&stderr=1&tail=400", &raw); err != nil {
		return nil, err
	}
	text := strings.TrimRight(logText(raw, details.Config.Tty), "\n")
	if text == "" {
		return nil, nil
	}
	return strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n"), nil
}

// field is one line of an info tab.
type field struct{ label, value string }

// containerFields reads the inspect data the info tab shows.
func (e engineClient) containerFields(ctx context.Context, c container, now int64) ([]field, error) {
	var info struct {
		ID, Path string
		Args     []string
		Config   struct {
			Image, WorkingDir string
			Env               []string
		}
		HostConfig struct {
			RestartPolicy struct{ Name string }
		}
	}
	if err := e.get(ctx, "/containers/"+url.PathEscape(c.ID)+"/json", &info); err != nil {
		return nil, err
	}
	command := append([]string{info.Path}, info.Args...)
	for i, arg := range command {
		if arg == "" || strings.ContainsAny(arg, " \t\n\r\"\\") {
			command[i] = strconv.Quote(arg)
		}
	}
	var ports, mounts, nets []string
	for _, p := range c.Ports {
		ports = append(ports, fmt.Sprintf("localhost:%d → %d/%s", p.Public, p.Private, p.Type))
	}
	for _, m := range c.Mounts {
		source := m.Source
		if m.Name != "" {
			source = m.Name
		}
		mounts = append(mounts, source+" → "+m.Destination)
	}
	for _, n := range c.Networks {
		nets = append(nets, strings.TrimSuffix(n.Network+" · "+n.IP, " · "))
	}
	fields := []field{
		{"ID", info.ID},
		{tr("镜像", "Image"), info.Config.Image},
		{tr("命令", "Command"), strings.TrimSpace(strings.Join(command, " "))},
		{tr("工作目录", "Working directory"), info.Config.WorkingDir},
		{tr("创建", "Created"), since(now, c.Created)},
		{tr("端口", "Ports"), strings.Join(ports, "\n")},
		{tr("网络", "Networks"), strings.Join(nets, "\n")},
		{tr("挂载", "Mounts"), strings.Join(mounts, "\n")},
		{tr("重启策略", "Restart policy"), info.HostConfig.RestartPolicy.Name},
		{tr("环境变量", "Environment"), strings.Join(info.Config.Env, "\n")},
		{"Compose", c.Project},
	}
	return compact(fields), nil
}

// imageFields reads the inspect data the image info tab shows.
func (e engineClient) imageFields(ctx context.Context, img image, now int64) ([]field, error) {
	var info struct {
		ID, Os, Architecture, Variant string
		Config                        struct {
			Cmd, Entrypoint, Env []string
			WorkingDir, User     string
		}
	}
	if err := e.get(ctx, "/images/"+url.PathEscape(img.ID)+"/json", &info); err != nil {
		return nil, err
	}
	platform := info.Os + "/" + info.Architecture
	if info.Variant != "" {
		platform += "/" + info.Variant
	}
	return compact([]field{
		{"ID", info.ID},
		{tr("标签", "Tags"), strings.Join(img.Tags, "\n")},
		{tr("平台", "Platform"), strings.Trim(platform, "/")},
		{tr("大小", "Size"), displaySize(img.Size)},
		{tr("创建", "Created"), since(now, img.Created)},
		{"Entrypoint", strings.Join(info.Config.Entrypoint, " ")},
		{"Cmd", strings.Join(info.Config.Cmd, " ")},
		{tr("工作目录", "Working directory"), info.Config.WorkingDir},
		{tr("用户", "User"), info.Config.User},
		{tr("环境变量", "Environment"), strings.Join(info.Config.Env, "\n")},
	}), nil
}

// compact drops the fields that have nothing to show.
func compact(fields []field) []field {
	out := fields[:0]
	for _, f := range fields {
		if f.value != "" {
			out = append(out, f)
		}
	}
	return out
}

// responseError turns a failed response into the daemon's own message.
func responseError(res *http.Response) error {
	var problem struct{ Message string }
	_ = json.NewDecoder(io.LimitReader(res.Body, 65536)).Decode(&problem)
	if problem.Message != "" {
		return fmt.Errorf("%s", problem.Message)
	}
	return fmt.Errorf("Kiri returned HTTP %d", res.StatusCode)
}
