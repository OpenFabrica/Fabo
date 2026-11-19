//go:build linux || darwin

package main

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"
)

// mcpProtocol is the newest Model Context Protocol revision this server
// speaks; a client asking for another gets this one, as the spec allows.
const mcpProtocol = "2025-06-18"

// mcpInstructions reach every agent that connects: the one habit Fabo most
// wants from them is to reuse the images already here.
const mcpInstructions = `Fabo runs this machine's containers. Before writing a compose file, a docker run or a Dockerfile FROM, call find_local_images for the image's name and reuse a local tag when it meets the need (same major version, a suitable variant such as alpine or debian); pull a new tag only when none does, and say why. Never swap an image a project names for another variant silently. Running containers with published ports answer at the local_url list_containers gives.`

// mcpCommand is `fabo mcp`: a Model Context Protocol server on standard
// input and output that lets an agent read this installation's engine.
// Every tool only reads; changes stay with docker and the window.
func mcpCommand(args []string) error {
	if len(args) != 0 {
		return errors.New("usage: fabo mcp")
	}
	in, err := locate()
	if err != nil {
		return err
	}
	server := &mcpServer{
		client: newCommandClient(in.socket()),
		names:  func() (string, bool) { return machineNames(in) },
	}
	return server.serve(os.Stdin, os.Stdout)
}

// mcpServer answers one client. names reports the suffix local addresses
// carry and whether the certificate is trusted.
type mcpServer struct {
	client *commandClient
	names  func() (suffix string, https bool)
}

type mcpRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type mcpError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// serve reads one JSON-RPC message per line and writes one answer per
// request; notifications get none. It ends when the client closes input.
func (s *mcpServer) serve(in io.Reader, out io.Writer) error {
	reader := bufio.NewReaderSize(in, 1<<20)
	encoder := json.NewEncoder(out)
	for {
		line, err := reader.ReadBytes('\n')
		if len(strings.TrimSpace(string(line))) > 0 {
			if answer := s.handle(line); answer != nil {
				if err := encoder.Encode(answer); err != nil {
					return err
				}
			}
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// handle turns one message into its answer, or nil for a notification.
func (s *mcpServer) handle(line []byte) any {
	var req mcpRequest
	if err := json.Unmarshal(line, &req); err != nil {
		return map[string]any{"jsonrpc": "2.0", "id": nil, "error": mcpError{-32700, "parse error"}}
	}
	if len(req.ID) == 0 || string(req.ID) == "null" {
		return nil
	}
	reply := func(result any) any { return map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result} }
	fail := func(code int, message string) any {
		return map[string]any{"jsonrpc": "2.0", "id": req.ID, "error": mcpError{code, message}}
	}
	switch req.Method {
	case "initialize":
		return reply(map[string]any{
			"protocolVersion": mcpProtocol,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "fabo", "version": "1"},
			"instructions":    mcpInstructions,
		})
	case "ping":
		return reply(map[string]any{})
	case "tools/list":
		return reply(map[string]any{"tools": mcpTools})
	case "tools/call":
		var call struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &call); err != nil {
			return fail(-32602, "invalid params")
		}
		result, err := s.call(call.Name, call.Arguments)
		if errors.Is(err, errUnknownTool) {
			return fail(-32602, err.Error())
		}
		// A tool that fails says so in its result, for the agent to read.
		if err != nil {
			return reply(map[string]any{"content": []any{map[string]any{"type": "text", "text": err.Error()}}, "isError": true})
		}
		text, _ := json.MarshalIndent(result, "", "  ")
		return reply(map[string]any{"content": []any{map[string]any{"type": "text", "text": string(text)}}, "structuredContent": result})
	}
	return fail(-32601, "method not found: "+req.Method)
}

var errUnknownTool = errors.New("unknown tool")

// mcpTool describes one tool; every one is read-only.
func mcpTool(name, description string, properties map[string]any, required ...string) map[string]any {
	schema := map[string]any{"type": "object", "properties": properties}
	if len(required) > 0 {
		schema["required"] = required
	}
	return map[string]any{
		"name": name, "description": description, "inputSchema": schema,
		"annotations": map[string]any{"readOnlyHint": true, "openWorldHint": false},
	}
}

var mcpTools = []map[string]any{
	mcpTool("engine_info", "The engine's version and state, its containers and images, and how local addresses are named.", map[string]any{}),
	mcpTool("list_containers", "Containers with their state, image, Compose project and service, published ports and local_url (the address a running container with published ports answers at).",
		map[string]any{
			"project": map[string]any{"type": "string", "description": "only this Compose project"},
			"running": map[string]any{"type": "boolean", "description": "only running containers"},
		}),
	mcpTool("container_logs", "The last lines a container wrote, optionally only those containing text or of one level.",
		map[string]any{
			"container": map[string]any{"type": "string", "description": "name or ID"},
			"tail":      map[string]any{"type": "integer", "description": "how many lines to read, 1-5000, default 200"},
			"grep":      map[string]any{"type": "string", "description": "keep lines containing this, case-insensitively"},
			"level":     map[string]any{"type": "string", "enum": []string{"error", "warn", "info", "debug"}, "description": "keep lines of this level"},
		}, "container"),
	mcpTool("compose_projects", "Compose projects: their services and containers, directory, config files, Compose version and local addresses.",
		map[string]any{"project": map[string]any{"type": "string", "description": "only this project"}}),
	mcpTool("find_local_images", "Images already on this machine, grouped by repository: the tags each image carries, its size, age and the containers using it. Call it before pulling, to reuse a local tag.",
		map[string]any{"name": map[string]any{"type": "string", "description": "part of the repository name, e.g. postgres; empty lists every image"}}),
	mcpTool("image_scan", "The latest vulnerability scan of a local image, if it has been scanned.",
		map[string]any{"image": map[string]any{"type": "string", "description": "image reference, e.g. postgres:17"}}, "image"),
}

// call runs one tool with its JSON arguments.
func (s *mcpServer) call(name string, raw json.RawMessage) (any, error) {
	var args struct {
		Project   string `json:"project"`
		Running   bool   `json:"running"`
		Container string `json:"container"`
		Tail      int    `json:"tail"`
		Grep      string `json:"grep"`
		Level     string `json:"level"`
		Name      string `json:"name"`
		Image     string `json:"image"`
	}
	if len(raw) > 0 && string(raw) != "null" {
		if err := json.Unmarshal(raw, &args); err != nil {
			return nil, fmt.Errorf("invalid arguments: %w", err)
		}
	}
	switch name {
	case "engine_info":
		return s.engineInfo()
	case "list_containers":
		return s.listContainers(args.Project, args.Running)
	case "container_logs":
		return s.containerLogs(args.Container, args.Tail, args.Grep, args.Level)
	case "compose_projects":
		return s.composeProjects(args.Project)
	case "find_local_images":
		return s.findLocalImages(args.Name)
	case "image_scan":
		return s.imageScan(args.Image)
	}
	return nil, fmt.Errorf("%w %q", errUnknownTool, name)
}

// apiContainer is what the tools read of /containers/json.
type apiContainer struct {
	ID, Image, ImageID, State, Status string
	Names                             []string
	Created                           int64
	Labels                            map[string]string
	Ports                             []struct {
		Type                    string
		PrivatePort, PublicPort int
	}
}

func (c apiContainer) name() string {
	if len(c.Names) == 0 {
		return c.ID[:min(12, len(c.ID))]
	}
	return strings.TrimPrefix(c.Names[0], "/")
}

// localAddresses holds what local addresses are built from.
type localAddresses struct {
	suffix, scheme string
	port           int
}

// addresses reads the machine's names and the router's ports once a call.
func (s *mcpServer) addresses() localAddresses {
	suffix, https := s.names()
	a := localAddresses{suffix: suffix, scheme: "http"}
	var router struct {
		HTTP, HTTPS struct {
			Port int `json:"port"`
		}
	}
	_ = s.client.request("GET", "/fabo/domains", nil, &router)
	a.port = router.HTTP.Port
	if https {
		a.scheme, a.port = "https", router.HTTPS.Port
	}
	return a
}

// localURL is the desktop's rule, which is Kiri's: a running container with
// published ports answers at the first name of dev.fabo.domain, else its
// Compose service.project, else its name, under the machine's suffix, with
// the router's port when it is not the scheme's default.
func (a localAddresses) localURL(c apiContainer) string {
	if c.State != "running" || len(c.Ports) == 0 {
		return ""
	}
	name := c.name()
	if project, service := c.Labels["com.docker.compose.project"], c.Labels["com.docker.compose.service"]; project != "" && service != "" {
		name = service + "." + project
	}
	if chosen := strings.TrimSpace(strings.Split(c.Labels["dev.fabo.domain"], ",")[0]); chosen != "" {
		name = strings.TrimSuffix(strings.TrimSuffix(chosen, ".fabo.localhost"), ".fabo.local")
	}
	addr := a.scheme + "://" + name + "." + a.suffix
	if standard := map[string]int{"http": 80, "https": 443}[a.scheme]; a.port != 0 && a.port != standard {
		addr += ":" + strconv.Itoa(a.port)
	}
	return addr
}

func (s *mcpServer) containers() ([]apiContainer, error) {
	var rows []apiContainer
	err := s.client.request("GET", "/containers/json?all=1", nil, &rows)
	return rows, err
}

// containerSummary is one row of list_containers.
type containerSummary struct {
	Name     string   `json:"name"`
	ID       string   `json:"id"`
	Image    string   `json:"image"`
	State    string   `json:"state"`
	Status   string   `json:"status"`
	Project  string   `json:"project,omitempty"`
	Service  string   `json:"service,omitempty"`
	Ports    []string `json:"ports,omitempty"`
	LocalURL string   `json:"local_url,omitempty"`
}

func summarize(c apiContainer, a localAddresses) containerSummary {
	row := containerSummary{Name: c.name(), ID: c.ID[:min(12, len(c.ID))], Image: c.Image, State: c.State, Status: c.Status,
		Project: c.Labels["com.docker.compose.project"], Service: c.Labels["com.docker.compose.service"], LocalURL: a.localURL(c)}
	seen := map[string]bool{}
	for _, p := range c.Ports {
		if p.PublicPort == 0 {
			continue
		}
		// A port published on IPv4 and IPv6 arrives twice.
		if port := fmt.Sprintf("%d->%d/%s", p.PublicPort, p.PrivatePort, p.Type); !seen[port] {
			seen[port] = true
			row.Ports = append(row.Ports, port)
		}
	}
	return row
}

func (s *mcpServer) listContainers(project string, running bool) (any, error) {
	rows, err := s.containers()
	if err != nil {
		return nil, err
	}
	a := s.addresses()
	out := []containerSummary{}
	for _, c := range rows {
		if (project == "" || c.Labels["com.docker.compose.project"] == project) && (!running || c.State == "running") {
			out = append(out, summarize(c, a))
		}
	}
	slices.SortFunc(out, func(x, y containerSummary) int { return strings.Compare(x.Project+"/"+x.Name, y.Project+"/"+y.Name) })
	return map[string]any{"containers": out}, nil
}

// engineInfo is the engine_info tool: the engine's versions and state from
// /info, and how local addresses are named. ServerVersion is the Docker
// Engine version; FaboVersion is Fabo's release, which an engine older than
// that field reported as ServerVersion.
func (s *mcpServer) engineInfo() (any, error) {
	var info map[string]any
	if err := s.client.request("GET", "/info", nil, &info); err != nil {
		return nil, err
	}
	a := s.addresses()
	result := map[string]any{"local_address_suffix": a.suffix, "local_address_scheme": a.scheme}
	for _, key := range []string{"ServerVersion", "FaboVersion", "Containers", "ContainersRunning", "Images", "NCPU", "MemTotal", "FaboVmAwake", "FaboStopped"} {
		if v, ok := info[key]; ok {
			result[key] = v
		}
	}
	if _, ok := result["FaboVersion"]; !ok && result["ServerVersion"] != nil {
		result["FaboVersion"] = result["ServerVersion"]
	}
	return result, nil
}

// Log levels as the desktop tells them apart: by whichever marking word
// comes first in the line.
var mcpLogWords = []struct{ word, level string }{
	{"error", "error"}, {"fatal", "error"}, {"panic", "error"}, {"crit", "error"}, {"exception", "error"},
	{"emerg", "error"}, {"[err", "error"}, {"level=err", "error"}, {" err ", "error"},
	{"warn", "warn"}, {"info", "info"}, {"notice", "info"}, {"debug", "debug"}, {"trace", "debug"},
}

func lineLevel(line string) string {
	lower := strings.ToLower(line)
	level, at := "", len(lower)
	for _, w := range mcpLogWords {
		if i := strings.Index(lower, w.word); i >= 0 && i < at {
			level, at = w.level, i
		}
	}
	return level
}

func (s *mcpServer) containerLogs(container string, tail int, grep, level string) (any, error) {
	if container == "" {
		return nil, errors.New("container is required")
	}
	if tail <= 0 {
		tail = 200
	}
	tail = min(tail, 5000)
	var inspect struct {
		Config struct{ Tty bool }
	}
	if err := s.client.request("GET", "/containers/"+url.PathEscape(container)+"/json", nil, &inspect); err != nil {
		return nil, err
	}
	var raw []byte
	if err := s.client.request("GET", "/containers/"+url.PathEscape(container)+"/logs?stdout=1&stderr=1&tail="+strconv.Itoa(tail), nil, &raw); err != nil {
		return nil, err
	}
	text := string(raw)
	if !inspect.Config.Tty {
		text = demultiplex(raw)
	}
	query := strings.ToLower(grep)
	lines := []string{}
	for _, line := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" && len(lines) == 0 {
			continue
		}
		if (query == "" || strings.Contains(strings.ToLower(line), query)) && (level == "" || lineLevel(line) == level) {
			lines = append(lines, line)
		}
	}
	return map[string]any{"container": container, "lines": lines, "count": len(lines)}, nil
}

// demultiplex joins the frames of a non-TTY stream: an 8-byte header with
// the payload's length before each piece of stdout or stderr.
func demultiplex(raw []byte) string {
	var out strings.Builder
	for len(raw) >= 8 {
		size := int(binary.BigEndian.Uint32(raw[4:8]))
		raw = raw[8:]
		if size > len(raw) {
			size = len(raw)
		}
		out.Write(raw[:size])
		raw = raw[size:]
	}
	return out.String()
}

func (s *mcpServer) composeProjects(only string) (any, error) {
	rows, err := s.containers()
	if err != nil {
		return nil, err
	}
	a := s.addresses()
	type project struct {
		Name           string             `json:"name"`
		Running        int                `json:"running"`
		WorkingDir     string             `json:"working_dir,omitempty"`
		ConfigFiles    []string           `json:"config_files,omitempty"`
		ComposeVersion string             `json:"compose_version,omitempty"`
		Services       []string           `json:"services"`
		Containers     []containerSummary `json:"containers"`
	}
	byName := map[string]*project{}
	for _, c := range rows {
		name := c.Labels["com.docker.compose.project"]
		if name == "" || only != "" && name != only {
			continue
		}
		p := byName[name]
		if p == nil {
			p = &project{Name: name, Services: []string{}}
			byName[name] = p
		}
		p.WorkingDir = cmpOr(p.WorkingDir, c.Labels["com.docker.compose.project.working_dir"])
		p.ComposeVersion = cmpOr(p.ComposeVersion, c.Labels["com.docker.compose.version"])
		if p.ConfigFiles == nil {
			for _, f := range strings.Split(c.Labels["com.docker.compose.project.config_files"], ",") {
				if f = strings.TrimSpace(f); f != "" {
					p.ConfigFiles = append(p.ConfigFiles, f)
				}
			}
		}
		if service := c.Labels["com.docker.compose.service"]; service != "" && !slices.Contains(p.Services, service) {
			p.Services = append(p.Services, service)
		}
		if c.State == "running" {
			p.Running++
		}
		p.Containers = append(p.Containers, summarize(c, a))
	}
	out := []*project{}
	for _, p := range byName {
		slices.Sort(p.Services)
		out = append(out, p)
	}
	slices.SortFunc(out, func(x, y *project) int { return strings.Compare(x.Name, y.Name) })
	return map[string]any{"projects": out}, nil
}

// familiarName drops Docker Hub's registry and library prefixes, as docker
// images shows names: index.docker.io/library/postgres:17 is postgres:17,
// the name an agent writes in a compose file.
func familiarName(ref string) string {
	for _, prefix := range []string{"index.docker.io/library/", "docker.io/library/", "index.docker.io/", "docker.io/"} {
		if rest, ok := strings.CutPrefix(ref, prefix); ok {
			return rest
		}
	}
	return ref
}

// cmpOr is the first non-empty string.
func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func (s *mcpServer) findLocalImages(name string) (any, error) {
	var images []struct {
		ID       string `json:"Id"`
		RepoTags []string
		Size     int64
		Created  int64
	}
	if err := s.client.request("GET", "/images/json", nil, &images); err != nil {
		return nil, err
	}
	rows, err := s.containers()
	if err != nil {
		return nil, err
	}
	users := map[string][]string{}
	for _, c := range rows {
		users[c.ImageID] = append(users[c.ImageID], c.name())
	}
	type localImage struct {
		Tags    []string `json:"tags"`
		ID      string   `json:"id"`
		SizeMB  float64  `json:"size_mb"`
		Created string   `json:"created"`
		UsedBy  []string `json:"used_by"`
	}
	query := strings.ToLower(name)
	groups := map[string][]localImage{}
	for _, img := range images {
		// One image can carry tags of several repositories; it is listed
		// under each, with only that repository's tags.
		byRepo := map[string][]string{}
		for _, tag := range img.RepoTags {
			if tag == "<none>:<none>" {
				continue
			}
			tag = familiarName(tag)
			repo := tag
			if i := strings.LastIndex(tag, ":"); i > strings.LastIndex(tag, "/") {
				repo = tag[:i]
			}
			if query == "" || strings.Contains(strings.ToLower(repo), query) {
				byRepo[repo] = append(byRepo[repo], tag)
			}
		}
		for repo, tags := range byRepo {
			slices.Sort(tags)
			groups[repo] = append(groups[repo], localImage{
				Tags: tags, ID: strings.TrimPrefix(img.ID, "sha256:")[:12], SizeMB: float64(img.Size*10/(1<<20)) / 10,
				Created: time.Unix(img.Created, 0).UTC().Format("2006-01-02"), UsedBy: append([]string{}, users[img.ID]...),
			})
		}
	}
	type group struct {
		Repository string       `json:"repository"`
		Images     []localImage `json:"images"`
	}
	out := []group{}
	for repo, list := range groups {
		slices.SortFunc(list, func(x, y localImage) int { return strings.Compare(y.Created, x.Created) })
		out = append(out, group{repo, list})
	}
	slices.SortFunc(out, func(x, y group) int { return strings.Compare(x.Repository, y.Repository) })
	return map[string]any{"repositories": out}, nil
}

func (s *mcpServer) imageScan(image string) (any, error) {
	if image == "" {
		return nil, errors.New("image is required")
	}
	var scan any
	err := s.client.request("GET", "/fabo/images/scan?name="+url.QueryEscape(image), nil, &scan)
	var missing *commandError
	if errors.As(err, &missing) && missing.status == 404 {
		return map[string]any{"image": image, "scanned": false}, nil
	}
	if err != nil {
		return nil, err
	}
	if scan == nil {
		return map[string]any{"image": image, "scanned": false}, nil
	}
	return map[string]any{"image": image, "scanned": true, "scan": scan}, nil
}
