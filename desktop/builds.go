package main

// Builds: BuildKit's history of docker builds on Fabo, read through
// `docker buildx history` (ls, inspect, logs, rm) as upstream's builds.rs
// does. Only builders whose BuildKit container already exists on Fabo's
// engine are asked: asking any other would make buildx create one and pull
// moby/buildkit. Asking a stopped or paused one starts it, which wakes the
// VM, so the window asks only while the Builds page is open.

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strings"
	"time"
)

// buildLogCap is how much output one step keeps, as upstream's.
const buildLogCap = 256 * 1024

// buildListCap is how many builds the list keeps, newest first.
const buildListCap = 300

// runningLogsWait is how long a running build's progress is read: `history
// logs` follows a running build until it ends, so the steps so far are
// what arrived by then.
const runningLogsWait = 2 * time.Second

// buildRecord is one build in the history list.
type buildRecord struct {
	Builder, ID, Name, Status string
	Started, Completed        time.Time
	Total, Done, Cached       int
}

// title is what the list and the panel call a build: its name, else its ID
// (a classic POST /build has no name).
func (b buildRecord) title() string { return cmp.Or(b.Name, b.ID) }

// key identifies a build across builders.
func (b buildRecord) key() string { return b.Builder + "/" + b.ID }

// buildKind sorts a status into "ok", "fail", "run" or "off" (canceled or
// unknown), whether it is ls's "Completed" or inspect's "completed".
func buildKind(status string) string {
	s := strings.ToLower(status)
	switch {
	case strings.HasPrefix(s, "complet"):
		return "ok"
	case strings.HasPrefix(s, "error"), strings.HasPrefix(s, "failed"):
		return "fail"
	case strings.HasPrefix(s, "running"):
		return "run"
	}
	return "off"
}

// buildStep is one vertex of a build, in the order it ran.
type buildStep struct {
	Name               string
	Started, Completed time.Time
	Cached             bool
	Error, Log         string
}

// state is how the step went: "fail", "cached", "skip" (never started),
// "run" or "ok".
func (st buildStep) state() string {
	switch {
	case st.Error != "":
		return "fail"
	case st.Cached:
		return "cached"
	case st.Started.IsZero():
		return "skip"
	case st.Completed.IsZero():
		return "run"
	}
	return "ok"
}

// dockerfileStep matches the steps a Dockerfile names, "[stage 3/9] …" or
// "[2/4] …"; the rest is BuildKit's own work.
var dockerfileStep = regexp.MustCompile(`^\[([^\]]* )?\d+/\d+\]`)

// internal reports whether the step is BuildKit's own work rather than a
// Dockerfile line.
func (st buildStep) internal() bool { return !dockerfileStep.MatchString(st.Name) }

// buildWarning is one of the build checks' warnings.
type buildWarning struct {
	Text, Detail, URL string
	Line              int
}

// buildFailure is why a build failed: the message, the step and the
// Dockerfile lines around it.
type buildFailure struct {
	Message, Step, Source string
}

// buildDetail is one build: the record, its steps and its warnings.
type buildDetail struct {
	buildRecord
	Context, Dockerfile string
	Error               *buildFailure
	Steps               []buildStep
	Warnings            []buildWarning
	// listStatus is the list's status when this was read, so a change in
	// the list reads the build again.
	listStatus string
}

// kind is the build's state, where a build stopped with Ctrl-C (an error
// saying it was canceled) counts as canceled.
func (d *buildDetail) kind() string {
	if d.Error != nil && canceled.MatchString(d.Error.Message) {
		return "off"
	}
	return buildKind(d.Status)
}

var canceled = regexp.MustCompile(`(?i)\bcancel`)

// buildLayer is one line of an image's history with the layer it made.
type buildLayer struct {
	By     string
	Size   int64
	Digest string
	// Empty lines change only the image's settings; Base ones came with
	// the base image.
	Empty, Base bool
}

// imageRunConfig is how an image runs, from its config.
type imageRunConfig struct {
	Cmd, Entrypoint, Env, Ports, Volumes, Healthcheck []string
	WorkingDir, User, StopSignal                      string
	Labels                                            map[string]string
}

// buildImage is one platform's image a build made.
type buildImage struct {
	Platform     string
	Size         int64
	ConfigDigest string
	Layers       []buildLayer
	Config       imageRunConfig
}

// buildLayers is what a build made: an image per platform, and the base
// images it was built from.
type buildLayers struct {
	Images []buildImage
	Bases  []string
}

// buildCLI runs `docker <args>` and returns its two streams.
type buildCLI func(ctx context.Context, args ...string) (stdout, stderr []byte, err error)

// buildHistory reads and changes the history through the docker CLI.
// have names the containers the engine has, so builders without a BuildKit
// container are left alone; context is the docker context that points at
// this engine.
type buildHistory struct {
	run     buildCLI
	have    map[string]bool
	context string
}

// buildx runs one buildx command; a failure carries buildx's own last
// error line.
func (h buildHistory) buildx(ctx context.Context, args ...string) ([]byte, []byte, error) {
	out, errOut, err := h.run(ctx, append([]string{"buildx"}, args...)...)
	if err != nil {
		return out, errOut, cliFailure(err, errOut)
	}
	return out, errOut, nil
}

// cliFailure turns a failed command into its last stderr line.
func cliFailure(err error, stderr []byte) error {
	lines := strings.Split(strings.TrimSpace(string(stderr)), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if line := strings.TrimSpace(lines[i]); line != "" {
			return errors.New(strings.TrimPrefix(line, "ERROR: "))
		}
	}
	return err
}

// jsonValues decodes every JSON value in data, one per line or run
// together, stopping at the first that does not parse.
func jsonValues[T any](data []byte) []T {
	var out []T
	dec := json.NewDecoder(bytes.NewReader(data))
	for {
		var v T
		if dec.Decode(&v) != nil {
			return out
		}
		out = append(out, v)
	}
}

// builders lists the docker-container builders on this engine's context
// whose BuildKit container exists, grouped by the node they share:
// builders on one node (as `fabo` and `fabo-build` are) read the same
// BuildKit. No buildx plugin means no builds yet, not an error.
func (h buildHistory) builders(ctx context.Context) ([][]string, error) {
	// Without the plugin, docker reads `buildx` as an unknown command.
	if _, _, err := h.buildx(ctx, "version"); err != nil {
		return nil, nil
	}
	out, _, err := h.buildx(ctx, "ls", "--format", "json")
	if err != nil {
		return nil, err
	}
	type node struct{ Name, Endpoint string }
	type builder struct {
		Name, Driver string
		Nodes        []node
	}
	var groups [][]string
	group := map[string]int{}
	for _, b := range jsonValues[builder](out) {
		if b.Driver != "docker-container" || strings.HasPrefix(b.Name, "-") {
			continue
		}
		for _, n := range b.Nodes {
			container := "buildx_buildkit_" + n.Name
			if n.Endpoint != h.context || !h.have[container] {
				continue
			}
			i, seen := group[container]
			if !seen {
				i = len(groups)
				group[container] = i
				groups = append(groups, nil)
			}
			groups[i] = append(groups[i], b.Name)
			break
		}
	}
	return groups, nil
}

// list returns the newest builds first, from every builder. A builder
// that cannot answer is skipped.
func (h buildHistory) list(ctx context.Context) ([]buildRecord, error) {
	groups, err := h.builders(ctx)
	if err != nil {
		return nil, err
	}
	var all []buildRecord
	for _, group := range groups {
		// Builders on one node list the same builds, but buildx keeps a
		// build's name, context and Dockerfile in the local state of the
		// builder that ran it: asked through another, the record has none.
		// So every builder of the node is asked, and each build is kept
		// once, from the builder that names it.
		at := map[string]int{}
		for _, b := range group {
			out, _, err := h.buildx(ctx, "history", "ls", "--builder", b, "--format", "json")
			if err != nil {
				continue
			}
			for _, r := range parseBuildList(b, out) {
				i, seen := at[r.ID]
				switch {
				case !seen:
					at[r.ID] = len(all)
					all = append(all, r)
				case all[i].Name == "" && r.Name != "":
					all[i] = r
				}
			}
		}
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].Started.After(all[j].Started) })
	if len(all) > buildListCap {
		all = all[:buildListCap]
	}
	return all, nil
}

// parseBuildList reads `history ls --format json`.
func parseBuildList(builder string, out []byte) []buildRecord {
	type raw struct {
		Ref         string     `json:"ref"`
		Name        string     `json:"name"`
		Status      string     `json:"status"`
		CreatedAt   *time.Time `json:"created_at"`
		CompletedAt *time.Time `json:"completed_at"`
		Total       int        `json:"total_steps"`
		Done        int        `json:"completed_steps"`
		Cached      int        `json:"cached_steps"`
	}
	var records []buildRecord
	for _, r := range jsonValues[raw](out) {
		id := r.Ref[strings.LastIndex(r.Ref, "/")+1:]
		if !validBuildID(id) {
			continue
		}
		b := buildRecord{Builder: builder, ID: id, Name: r.Name, Status: r.Status, Total: r.Total, Done: r.Done, Cached: r.Cached}
		if r.CreatedAt != nil {
			b.Started = *r.CreatedAt
		}
		if r.CompletedAt != nil {
			b.Completed = *r.CompletedAt
		}
		records = append(records, b)
	}
	return records
}

// validBuildID guards the arguments handed to buildx: a build ID is
// alphanumeric, as upstream checks.
func validBuildID(id string) bool {
	if id == "" {
		return false
	}
	for _, r := range id {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') {
			return false
		}
	}
	return true
}

// checkBuild rejects a builder or ID that could be read as a flag.
func checkBuild(builder, id string) error {
	if !validBuildID(id) || builder == "" || strings.HasPrefix(builder, "-") {
		return errors.New("not a build")
	}
	return nil
}

// buildInspect is `history inspect --format json`.
type buildInspect struct {
	Name, Ref, Context, Dockerfile, Status string
	StartedAt, CompletedAt                 *time.Time
	NumCompletedSteps, NumTotalSteps       int
	NumCachedSteps                         int
	Error                                  *struct {
		Message, Name string
		Sources       []byte
	}
	Materials   []struct{ URI string }
	Attachments []struct{ Digest, Type, Platform string }
}

// inspect reads one build's record.
func (h buildHistory) inspect(ctx context.Context, builder, id string) (*buildInspect, error) {
	out, _, err := h.buildx(ctx, "history", "inspect", "--builder", builder, id, "--format", "json")
	if err != nil {
		return nil, err
	}
	var info buildInspect
	if err := json.Unmarshal(out, &info); err != nil {
		return nil, err
	}
	return &info, nil
}

// detail reads one build: the record, then its steps and warnings from the
// raw progress stream, which buildx writes to stderr.
func (h buildHistory) detail(ctx context.Context, builder, id string) (*buildDetail, error) {
	if err := checkBuild(builder, id); err != nil {
		return nil, err
	}
	info, err := h.inspect(ctx, builder, id)
	if err != nil {
		return nil, err
	}
	logs := ctx
	if buildKind(info.Status) == "run" {
		var cancel context.CancelFunc
		logs, cancel = context.WithTimeout(ctx, runningLogsWait)
		defer cancel()
	}
	// A running build's read ends at the timeout with what arrived so far.
	_, raw, _ := h.buildx(logs, "history", "logs", "--builder", builder, "--progress", "rawjson", id)
	return buildDetailOf(builder, id, info, raw), nil
}

// buildDetailOf puts the record and the progress stream together.
func buildDetailOf(builder, id string, info *buildInspect, raw []byte) *buildDetail {
	d := &buildDetail{
		buildRecord: buildRecord{Builder: builder, ID: id, Name: info.Name, Status: info.Status,
			Total: info.NumTotalSteps, Done: info.NumCompletedSteps, Cached: info.NumCachedSteps},
		Context: info.Context, Dockerfile: info.Dockerfile,
	}
	if info.StartedAt != nil {
		d.Started = *info.StartedAt
	}
	if info.CompletedAt != nil {
		d.Completed = *info.CompletedAt
	}
	if e := info.Error; e != nil {
		d.Error = &buildFailure{Message: e.Message, Step: e.Name, Source: string(e.Sources)}
	}
	d.Steps, d.Warnings = parseBuildProgress(raw)
	return d
}

// parseBuildProgress reads BuildKit's rawjson progress: each vertex's
// earliest start, latest end, cache and error; each step's output; and
// the build checks' warnings. Steps come in the order they started, then
// the ones that never did (skipped after an error) in the order they
// first appeared.
func parseBuildProgress(raw []byte) ([]buildStep, []buildWarning) {
	type vertex struct {
		Digest, Name, Error string
		Started, Completed  *time.Time
		Cached              bool
	}
	type event struct {
		Vertexes []vertex
		Logs     []struct {
			Vertex string
			Data   []byte
		}
		Warnings []struct {
			Short  []byte
			Detail [][]byte
			URL    string
			Range  []struct{ Start struct{ Line int } }
		}
	}
	steps := map[string]*buildStep{}
	var started, seen []string
	logs := map[string]*strings.Builder{}
	var warnings []buildWarning
	for _, ev := range jsonValues[event](raw) {
		for _, v := range ev.Vertexes {
			if v.Digest == "" {
				continue
			}
			st := steps[v.Digest]
			if st == nil {
				st = &buildStep{Name: v.Name}
				steps[v.Digest] = st
				seen = append(seen, v.Digest)
			}
			if v.Started != nil && (st.Started.IsZero() || v.Started.Before(st.Started)) {
				if st.Started.IsZero() {
					started = append(started, v.Digest)
				}
				st.Started = *v.Started
			}
			if v.Completed != nil && v.Completed.After(st.Completed) {
				st.Completed = *v.Completed
			}
			st.Cached = st.Cached || v.Cached
			if v.Error != "" {
				st.Error = v.Error
			}
		}
		for _, l := range ev.Logs {
			text := logs[l.Vertex]
			if text == nil {
				text = &strings.Builder{}
				logs[l.Vertex] = text
			}
			if text.Len() < buildLogCap {
				text.Write(l.Data)
			}
		}
		for _, w := range ev.Warnings {
			details := make([]string, 0, len(w.Detail))
			for _, d := range w.Detail {
				details = append(details, string(d))
			}
			warning := buildWarning{Text: string(w.Short), Detail: strings.Join(details, " "), URL: w.URL}
			if len(w.Range) > 0 {
				warning.Line = w.Range[0].Start.Line
			}
			warnings = append(warnings, warning)
		}
	}
	order := started
	for _, d := range seen {
		if !slices.Contains(started, d) {
			order = append(order, d)
		}
	}
	out := make([]buildStep, 0, len(order))
	for _, d := range order {
		st := *steps[d]
		if text := logs[d]; text != nil {
			st.Log = text.String()
		}
		out = append(out, st)
	}
	return out, warnings
}

// imageManifest is the part of an image manifest the Layers tab reads.
type imageManifest struct {
	Config struct{ Digest string }
	Layers []struct {
		Size   int64
		Digest string
	}
}

// imageConfig is the part of an image config the Layers tab reads.
type imageConfig struct {
	Created      string
	OS           string
	Architecture string
	History      []struct {
		Created    string
		CreatedBy  string `json:"created_by"`
		EmptyLayer bool   `json:"empty_layer"`
	}
	Config struct {
		Cmd, Entrypoint, Env []string
		WorkingDir, User     string
		ExposedPorts         map[string]any
		Volumes              map[string]any
		Labels               map[string]string
		Healthcheck          *struct{ Test []string }
		StopSignal           string
	}
}

// basePrefix is how a build's provenance names a base image.
var basePrefix = regexp.MustCompile(`^pkg:docker/([^?]+)`)

// sha256Digest matches a full sha256 digest, the only attachment buildx is
// asked for.
var sha256Digest = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// layers reads the image a build made, per platform, from the manifest
// and config BuildKit keeps with the record. Builds that made no image
// (cache only, or failed) have none.
func (h buildHistory) layers(ctx context.Context, builder, id string) (*buildLayers, error) {
	if err := checkBuild(builder, id); err != nil {
		return nil, err
	}
	info, err := h.inspect(ctx, builder, id)
	if err != nil {
		return nil, err
	}
	attachment := func(digest string, into any) error {
		if !sha256Digest.MatchString(digest) {
			return errors.New("not a digest")
		}
		out, _, err := h.buildx(ctx, "history", "inspect", "attachment", "--builder", builder, id, digest)
		if err != nil {
			return err
		}
		return json.Unmarshal(out, into)
	}
	result := &buildLayers{}
	for _, m := range info.Materials {
		if base := basePrefix.FindStringSubmatch(m.URI); base != nil {
			result.Bases = append(result.Bases, base[1])
		}
	}
	for _, a := range info.Attachments {
		if a.Type != "application/vnd.oci.image.manifest.v1+json" && a.Type != "application/vnd.docker.distribution.manifest.v2+json" {
			continue
		}
		var manifest imageManifest
		if attachment(a.Digest, &manifest) != nil {
			continue
		}
		var config imageConfig
		_ = attachment(manifest.Config.Digest, &config)
		result.Images = append(result.Images, imageOf(manifest, config, a.Platform))
	}
	return result, nil
}

// imageOf pairs the config's history lines with the manifest's layers
// (lines marked empty_layer add none).
func imageOf(manifest imageManifest, config imageConfig, platform string) buildImage {
	img := buildImage{Platform: platform, ConfigDigest: manifest.Config.Digest}
	if img.Platform == "" {
		img.Platform = cmp.Or(config.OS, "linux") + "/" + cmp.Or(config.Architecture, "?")
	}
	for _, l := range manifest.Layers {
		img.Size += l.Size
	}
	// BuildKit dates every history line a build writes with the image's
	// own time (cached steps too), the classic builder with each step's:
	// a line dated well before the image (an hour) came with the base
	// image. The base's lines come first, so the first newer line ends
	// them: a COPY whose cache dates from an older build is still this
	// image's own.
	made, _ := time.Parse(time.RFC3339Nano, config.Created)
	base := !made.IsZero()
	next := 0
	for _, line := range config.History {
		l := buildLayer{By: strings.TrimSuffix(line.CreatedBy, " # buildkit"), Empty: line.EmptyLayer}
		if !l.Empty && next < len(manifest.Layers) {
			l.Size, l.Digest = manifest.Layers[next].Size, manifest.Layers[next].Digest
			next++
		}
		created, err := time.Parse(time.RFC3339Nano, line.Created)
		base = base && err == nil && created.Before(made.Add(-time.Hour))
		l.Base = base
		img.Layers = append(img.Layers, l)
	}
	// Layers the history doesn't describe (a config without history).
	for _, l := range manifest.Layers[next:] {
		img.Layers = append(img.Layers, buildLayer{Size: l.Size, Digest: l.Digest})
	}
	c := config.Config
	img.Config = imageRunConfig{Cmd: c.Cmd, Entrypoint: c.Entrypoint, Env: c.Env, WorkingDir: c.WorkingDir, User: c.User,
		Labels: c.Labels, StopSignal: c.StopSignal, Ports: sortedKeys(c.ExposedPorts), Volumes: sortedKeys(c.Volumes)}
	if c.Healthcheck != nil {
		img.Config.Healthcheck = c.Healthcheck.Test
	}
	return img
}

// sortedKeys lists a map's keys in order.
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// remove takes builds of one builder out of the history.
func (h buildHistory) remove(ctx context.Context, builder string, ids []string) error {
	for _, id := range ids {
		if err := checkBuild(builder, id); err != nil {
			return err
		}
	}
	_, _, err := h.buildx(ctx, append([]string{"history", "rm", "--builder", builder}, ids...)...)
	return err
}

// dockerCLIPath finds the docker CLI: PATH first, then the one this
// installation ships beside the window (the macOS bundle's Resources/bin,
// Windows' bin\). An app opened from the Finder has a short PATH, so the
// usual install places are tried after it.
func dockerCLIPath() string {
	if path, err := exec.LookPath("docker"); err == nil {
		return path
	}
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	dir := filepath.Dir(exe)
	candidates := []string{filepath.Join(dir, "..", "Resources", "bin", "docker"), "/usr/local/bin/docker", "/opt/homebrew/bin/docker"}
	if runtime.GOOS == "windows" {
		candidates = []string{filepath.Join(dir, "bin", "docker.exe")}
	}
	for _, path := range candidates {
		if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() {
			return path
		}
	}
	return ""
}

// runDockerCLI runs the docker CLI against this window's engine, named by
// DOCKER_HOST so the user's current context does not matter.
func runDockerCLI(ctx context.Context, args ...string) ([]byte, []byte, error) {
	docker := dockerCLIPath()
	if docker == "" {
		return nil, nil, errors.New(tr("找不到 docker 命令", "No docker CLI found"))
	}
	cmd := exec.CommandContext(ctx, docker, args...)
	hideConsole(cmd)
	endGroup(cmd)
	// Once the context ends, the output read so far is enough: a plugin
	// that outlives docker must not keep Run waiting on the pipes.
	cmd.WaitDelay = time.Second
	cmd.Env = slices.DeleteFunc(os.Environ(), func(v string) bool {
		return strings.HasPrefix(v, "DOCKER_HOST=") || strings.HasPrefix(v, "DOCKER_CONTEXT=")
	})
	if host := engineHost(); host != "" {
		cmd.Env = append(cmd.Env, "DOCKER_HOST="+host)
	}
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr, cmd.Stdin = &out, &errOut, nil
	err := cmd.Run()
	if err != nil && ctx.Err() != nil {
		err = fmt.Errorf("docker %s: %w", strings.Join(args[:min(len(args), 3)], " "), ctx.Err())
	}
	return out.Bytes(), errOut.Bytes(), err
}
