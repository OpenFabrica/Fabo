//go:build linux || darwin

package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"
)

const importUsage = "usage: fabo import [orbstack|desktop-linux] [--list | image|volume|container NAME]"

type importOptions struct {
	source, kind, name string
	list               bool
}

func parseImport(args []string) (importOptions, error) {
	o := importOptions{source: "orbstack"}
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		o.source = args[0]
		args = args[1:]
	}
	if o.source != "orbstack" && o.source != "desktop-linux" {
		return o, fmt.Errorf("unknown source %s", o.source)
	}
	switch {
	case len(args) == 0:
	case len(args) == 1 && args[0] == "--list":
		o.list = true
	case len(args) == 2 && (args[0] == "image" || args[0] == "volume" || args[0] == "container") && args[1] != "" && !strings.HasPrefix(args[1], "-"):
		o.kind, o.name = args[0], args[1]
	default:
		return o, errors.New(importUsage)
	}
	return o, nil
}

type importer struct {
	in             installation
	client         *commandClient
	docker, source string
	ctx            context.Context
}

func (i *importer) command(source bool, args ...string) *exec.Cmd {
	if source {
		args = append([]string{"--context", i.source}, args...)
	}
	cmd := exec.CommandContext(i.ctx, i.docker, args...)
	cmd.Env = i.in.dockerEnvironment()
	if source {
		cmd.Env = slices.DeleteFunc(cmd.Env, func(s string) bool {
			return strings.HasPrefix(s, "DOCKER_HOST=") || strings.HasPrefix(s, "DOCKER_CONTEXT=")
		})
	}
	return cmd
}
func (i *importer) output(source bool, args ...string) ([]byte, error) {
	cmd := i.command(source, args...)
	var out, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &stderr
	if e := cmd.Run(); e != nil {
		return nil, fmt.Errorf("docker %s: %w: %s", strings.Join(args, " "), e, strings.TrimSpace(stderr.String()))
	}
	return out.Bytes(), nil
}
func (i *importer) rows(source bool, args ...string) ([]map[string]any, error) {
	raw, e := i.output(source, args...)
	if e != nil {
		return nil, e
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var rows []map[string]any
	for {
		var row map[string]any
		e := dec.Decode(&row)
		if e == io.EOF {
			break
		}
		if e != nil {
			return nil, e
		}
		if row == nil {
			return nil, errors.New("docker returned a non-object inventory row")
		}
		rows = append(rows, row)
	}
	return rows, nil
}
func textField(m map[string]any, k string) string { s, _ := m[k].(string); return s }
func mapField(m map[string]any, k string) map[string]any {
	v, _ := m[k].(map[string]any)
	if v == nil {
		return map[string]any{}
	}
	return v
}
func anonymousVolume(s string) bool {
	if len(s) != 64 {
		return false
	}
	_, e := hex.DecodeString(s)
	return e == nil
}

type importItem struct {
	name            string
	have, anonymous bool
}

func (i *importer) inventory() (map[string][]importItem, error) {
	out := map[string][]importItem{}
	for _, kind := range []string{"image", "volume", "container"} {
		args := []string{"image", "ls", "--format", "{{json .}}"}
		key := ""
		if kind == "volume" {
			args = []string{"volume", "ls", "--format", "{{json .}}"}
			key = "Name"
		}
		if kind == "container" {
			args = []string{"ps", "-a", "--format", "{{json .}}"}
			key = "Names"
		}
		target, e := i.rows(false, args...)
		if e != nil {
			return nil, e
		}
		source, e := i.rows(true, args...)
		if e != nil {
			return nil, e
		}
		name := func(row map[string]any) string {
			if key != "" {
				return textField(row, key)
			}
			repo, tag := textField(row, "Repository"), textField(row, "Tag")
			if repo == "<none>" || tag == "<none>" || repo == "" || tag == "" {
				return ""
			}
			return repo + ":" + tag
		}
		have := map[string]bool{}
		for _, row := range target {
			have[name(row)] = true
		}
		for _, row := range source {
			n := name(row)
			if n != "" {
				out[kind] = append(out[kind], importItem{n, have[n], kind == "volume" && anonymousVolume(n)})
			}
		}
	}
	return out, nil
}

// A bounded pipeline; start both ends, close our pipe descriptors, and reap
// the producer even when the consumer fails before draining its input.
func pipeImport(producer, consumer *exec.Cmd) error {
	r, w, e := os.Pipe()
	if e != nil {
		return e
	}
	defer r.Close()
	defer w.Close()
	var srcErr, dstErr bytes.Buffer
	producer.Stdout = w
	producer.Stderr = &srcErr
	consumer.Stdin = r
	consumer.Stdout = io.Discard
	consumer.Stderr = &dstErr
	if e = consumer.Start(); e != nil {
		return e
	}
	if e = producer.Start(); e != nil {
		w.Close()
		r.Close()
		_ = consumer.Process.Kill()
		_ = consumer.Wait()
		return e
	}
	w.Close()
	r.Close()
	dst := consumer.Wait()
	if dst != nil {
		_ = producer.Process.Kill()
	}
	src := producer.Wait()
	if dst != nil {
		return fmt.Errorf("import destination: %w: %s", dst, strings.TrimSpace(dstErr.String()))
	}
	if src != nil {
		return fmt.Errorf("import source: %w: %s", src, strings.TrimSpace(srcErr.String()))
	}
	return nil
}
func (i *importer) image(name string) error {
	return pipeImport(i.command(true, "save", name), i.command(false, "load", "-q"))
}
func (i *importer) helperImage() (string, error) {
	for _, img := range []string{"alpine:latest", "busybox:latest", "alpine:3.22", "debian:trixie", "ubuntu:24.04"} {
		if _, e := i.output(true, "image", "inspect", img); e == nil {
			if _, e := i.output(false, "image", "inspect", img); e != nil {
				if e = i.image(img); e != nil {
					return "", e
				}
			}
			return img, nil
		}
	}
	return "", fmt.Errorf("%s has no alpine or busybox image to read volumes with; pull alpine there first", i.source)
}
func (i *importer) volume(name string) error {
	if _, err := i.output(true, "volume", "inspect", name); err != nil {
		return err
	}
	// Never mix a partial import into an existing target volume on a retry.
	var existing struct{ Volumes []map[string]any }
	if e := i.client.request("GET", "/volumes", nil, &existing); e != nil {
		return e
	}
	for _, v := range existing.Volumes {
		if textField(v, "Name") == name {
			return fmt.Errorf("volume %s already exists in Fabo; not overwriting it", name)
		}
	}
	image, e := i.helperImage()
	if e != nil {
		return e
	}
	if _, e = i.output(false, "volume", "create", name); e != nil {
		return e
	}
	nameHelper := fmt.Sprintf("fabo-import-%d-%d", os.Getpid(), time.Now().UnixNano())
	err := pipeImport(i.command(true, "run", "--name", nameHelper, "--rm", "--network", "none", "--label", "dev.fabo.import-helper=1", "-v", name+":/from:ro", image, "tar", "-C", "/from", "-cf", "-", "."), i.command(false, "run", "--name", nameHelper, "--rm", "-i", "--network", "none", "--label", "dev.fabo.import-helper=1", "-v", name+":/to", image, "tar", "-C", "/to", "-xpf", "-"))
	if err == nil {
		return nil
	}
	// Docker CLI cancellation does not guarantee the remote helper stopped.
	// Use a fresh bounded context to release both mounts before rollback.
	cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cleanup := *i
	cleanup.ctx = cleanupCtx
	_, _ = cleanup.output(true, "rm", "-f", nameHelper)
	_, _ = cleanup.output(false, "rm", "-f", nameHelper)
	if _, removeErr := cleanup.output(false, "volume", "rm", name); removeErr != nil {
		return fmt.Errorf("%w; partial target volume %s remains: %v", err, name, removeErr)
	}
	return err
}

func importContainerSpec(info map[string]any) map[string]any {
	cfg, hc := mapField(info, "Config"), mapField(info, "HostConfig")
	body, host := map[string]any{}, map[string]any{}
	for _, key := range strings.Fields("Image Cmd Entrypoint Env Labels WorkingDir User ExposedPorts Tty OpenStdin StopSignal Hostname") {
		if v, ok := cfg[key]; ok {
			body[key] = v
		}
	}
	for _, key := range strings.Fields("Binds PortBindings RestartPolicy Mounts NetworkMode Privileged CapAdd CapDrop ExtraHosts Tmpfs ReadonlyRootfs SecurityOpt Memory NanoCpus ShmSize PublishAllPorts") {
		if v, ok := hc[key]; ok && v != nil {
			host[key] = v
		}
	}
	networks := map[string]any{}
	short := textField(info, "Id")
	if len(short) > 12 {
		short = short[:12]
	}
	for name, ep := range mapField(mapField(info, "NetworkSettings"), "Networks") {
		endpoint, _ := ep.(map[string]any)
		aliases, _ := endpoint["Aliases"].([]any)
		kept := []any{}
		for _, a := range aliases {
			if a != short {
				kept = append(kept, a)
			}
		}
		networks[name] = map[string]any{"Aliases": kept}
	}
	body["HostConfig"] = host
	body["NetworkingConfig"] = map[string]any{"EndpointsConfig": networks}
	return body
}
func (i *importer) container(name string) error {
	rows, e := i.rows(true, "inspect", "--type", "container", "--format", "{{json .}}", name)
	if e != nil {
		return e
	}
	if len(rows) != 1 {
		return fmt.Errorf("%s: expected one source container", name)
	}
	info := rows[0]
	image := textField(mapField(info, "Config"), "Image")
	if image == "" {
		return errors.New("source container has no image")
	}
	if _, e := i.output(false, "image", "inspect", image); e != nil {
		if e = i.image(image); e != nil {
			return e
		}
	}
	for network := range mapField(mapField(info, "NetworkSettings"), "Networks") {
		if slices.Contains([]string{"bridge", "host", "none", "default"}, network) {
			continue
		}
		if _, e := i.output(false, "network", "inspect", network); e != nil {
			if _, e = i.output(false, "network", "create", network); e != nil {
				return e
			}
		}
	}
	return i.client.request("POST", "/containers/create?name="+url.QueryEscape(name), importContainerSpec(info), nil)
}
func importCommand(args []string) error {
	o, e := parseImport(args)
	if e != nil {
		return e
	}
	in, e := locate()
	if e != nil {
		return e
	}
	client, e := in.runningClient()
	if e != nil {
		return e
	}
	defer client.http.CloseIdleConnections()
	docker := in.dockerCLI()
	if docker == "" {
		return errors.New("no docker CLI found")
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	ctx, stop := context.WithTimeout(ctx, 30*time.Minute)
	defer stop()
	importer := &importer{in: in, client: client, docker: docker, source: o.source, ctx: ctx}
	endpoint, e := importer.output(true, "context", "inspect", o.source, "--format", "{{.Endpoints.docker.Host}}")
	if e != nil {
		return e
	}
	if source, ok := strings.CutPrefix(strings.TrimSpace(string(endpoint)), "unix://"); ok {
		canonical := func(s string) string {
			if real, e := filepath.EvalSymlinks(s); e == nil {
				return real
			}
			return filepath.Clean(s)
		}
		if canonical(source) == canonical(in.socket()) {
			return errors.New("the import source points at this Fabo engine")
		}
	}
	transfer := func(kind, name string) error {
		switch kind {
		case "image":
			return importer.image(name)
		case "volume":
			return importer.volume(name)
		case "container":
			return importer.container(name)
		}
		return errors.New(importUsage)
	}
	if o.kind != "" {
		if e := transfer(o.kind, o.name); e != nil {
			return e
		}
		fmt.Printf("%s %s: done\n", o.kind, o.name)
		return nil
	}
	inv, e := importer.inventory()
	if e != nil {
		return e
	}
	failed := 0
	for _, kind := range []string{"image", "volume", "container"} {
		if o.list {
			fmt.Printf("%ss:\n", kind)
		}
		for _, item := range inv[kind] {
			if o.list {
				suffix := ""
				if item.have {
					suffix = "  (already in Fabo)"
				}
				fmt.Printf("  %s%s\n", item.name, suffix)
				continue
			}
			if item.have || item.anonymous {
				continue
			}
			if e := transfer(kind, item.name); e != nil {
				failed++
				fmt.Printf("%s %s: %v\n", kind, item.name, e)
			} else {
				fmt.Printf("%s %s: done\n", kind, item.name)
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
		}
	}
	if failed > 0 {
		return fmt.Errorf("%d items could not be imported", failed)
	}
	return nil
}
