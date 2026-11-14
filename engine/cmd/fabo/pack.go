//go:build linux || darwin

package main

// fabo pack writes containers, with the changes made inside them, their
// volumes' files and their networks, into one file; fabo unpack recreates
// them in this engine or on another machine.

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	packUsage   = "usage: fabo pack [-o FILE] [--project NAME] [CONTAINER...]\n       fabo unpack [--start] FILE"
	packFormat  = "fabo-pack"
	packVersion = 1
	// PackExtension names the files fabo pack writes.
	packExtension = ".fabopack"
	// The helper container mounts volume i at packHelperRoot/i, so the
	// archive of that path holds entries named "i/…" and extracting it into
	// packHelperRoot fills the same volume again.
	packHelperRoot  = "/fabo-pack"
	packHelperLabel = "dev.fabo.pack-helper"
	maxManifest     = 16 << 20
)

// packManifest is the package's first entry: everything unpack needs before
// it reads the images and volume files that follow.
type packManifest struct {
	Format     string          `json:"format"`
	Version    int             `json:"version"`
	Created    time.Time       `json:"created"`
	Containers []packContainer `json:"containers"`
	Volumes    []packVolume    `json:"volumes"`
	Networks   []packNetwork   `json:"networks"`
}

// packContainer is one container: the committed image in images.tar and the
// body that creates it again.
type packContainer struct {
	Name        string         `json:"name"`
	Image       string         `json:"image"`
	SourceImage string         `json:"source_image"`
	Running     bool           `json:"running"`
	Spec        map[string]any `json:"spec"`
}

// packVolume is a volume's definition; Data names the entry holding its files,
// empty when they live outside the engine (a bind-backed or remote volume).
type packVolume struct {
	Name    string            `json:"name"`
	Driver  string            `json:"driver"`
	Labels  map[string]string `json:"labels,omitempty"`
	Options map[string]string `json:"options,omitempty"`
	Data    string            `json:"data,omitempty"`
}

// packNetwork is a user-defined network the containers join.
type packNetwork struct {
	Name     string            `json:"name"`
	Driver   string            `json:"driver"`
	Labels   map[string]string `json:"labels,omitempty"`
	Internal bool              `json:"internal,omitempty"`
}

// packCommand parses `fabo pack` and writes the package.
func packCommand(args []string) error {
	var out, project string
	var names []string
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case (a == "-o" || a == "--output" || a == "--project") && i+1 < len(args):
			if a == "--project" {
				project = args[i+1]
			} else {
				out = args[i+1]
			}
			i++
		case strings.HasPrefix(a, "-"):
			return errors.New(packUsage)
		default:
			names = append(names, a)
		}
	}
	if (project == "") == (len(names) == 0) {
		return errors.New(packUsage)
	}
	in, err := locate()
	if err != nil {
		return err
	}
	client, err := in.runningClient()
	if err != nil {
		return err
	}
	defer client.http.CloseIdleConnections()
	if project != "" {
		if names, err = projectContainers(client, project); err != nil {
			return err
		}
	}
	if out == "" {
		out = cmpOr(project, names[0]) + "-" + time.Now().Format("20060102-150405") + packExtension
	}
	return packContainers(client, names, out, os.Stdout)
}

// unpackCommand parses `fabo unpack` and restores the package.
func unpackCommand(args []string) error {
	start, file := false, ""
	for _, a := range args {
		switch {
		case a == "--start":
			start = true
		case strings.HasPrefix(a, "-") || file != "":
			return errors.New(packUsage)
		default:
			file = a
		}
	}
	if file == "" {
		return errors.New(packUsage)
	}
	in, err := locate()
	if err != nil {
		return err
	}
	client, err := in.runningClient()
	if err != nil {
		return err
	}
	defer client.http.CloseIdleConnections()
	return unpackFile(client, file, start, os.Stdout)
}

// projectContainers names every container of a Compose project.
func projectContainers(c *commandClient, project string) ([]string, error) {
	var rows []struct {
		Names  []string
		Labels map[string]string
	}
	if err := c.request("GET", "/containers/json?all=1", nil, &rows); err != nil {
		return nil, err
	}
	var names []string
	for _, row := range rows {
		if row.Labels["com.docker.compose.project"] == project && len(row.Names) > 0 {
			names = append(names, strings.TrimPrefix(row.Names[0], "/"))
		}
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("no containers in project %s", project)
	}
	slices.Sort(names)
	return names, nil
}

// packRepository turns a container name into an image repository: lower
// case, every other run of characters one "-".
func packRepository(name string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(name) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
			dash = false
		} else if !dash && b.Len() > 0 {
			b.WriteByte('-')
			dash = true
		}
	}
	return "fabo-pack/" + cmpOr(strings.TrimSuffix(b.String(), "-"), "container")
}

// hostBind reports whether a Binds entry's source is a host path rather
// than a volume name.
func hostBind(bind string) bool { return strings.HasPrefix(bind, "/") }

// packSpec turns an inspected container into the body that recreates it with
// image. Volume mounts are rebuilt from Mounts as "name:destination" binds,
// so an anonymous volume keeps its files too; host binds stay as they are.
func packSpec(info map[string]any, image string, names map[string]string) (map[string]any, []string, error) {
	spec := importContainerSpec(info)
	spec["Image"] = image
	host := spec["HostConfig"].(map[string]any)
	var binds []any
	if list, ok := host["Binds"].([]any); ok {
		for _, b := range list {
			if s, _ := b.(string); hostBind(s) {
				binds = append(binds, s)
			}
		}
	}
	if list, ok := host["Mounts"].([]any); ok {
		var kept []any
		for _, m := range list {
			if mount, _ := m.(map[string]any); textField(mount, "Type") != "volume" {
				kept = append(kept, m)
			}
		}
		host["Mounts"] = kept
	}
	var volumes []string
	mounts, _ := info["Mounts"].([]any)
	for _, m := range mounts {
		mount, _ := m.(map[string]any)
		name := textField(mount, "Name")
		if textField(mount, "Type") != "volume" || name == "" {
			continue
		}
		bind := name + ":" + textField(mount, "Destination")
		if rw, ok := mount["RW"].(bool); ok && !rw {
			bind += ":ro"
		}
		binds = append(binds, bind)
		volumes = append(volumes, name)
	}
	host["Binds"] = binds
	// A container sharing another's network names it by ID; the package
	// carries names, and the other container must travel with it.
	if mode := textField(host, "NetworkMode"); strings.HasPrefix(mode, "container:") {
		target := strings.TrimPrefix(mode, "container:")
		name, ok := names[target]
		if !ok {
			for id, n := range names {
				if strings.HasPrefix(id, target) || n == target {
					name, ok = n, true
				}
			}
		}
		if !ok {
			return nil, nil, fmt.Errorf("%s shares the network of container %s; pack that container too", strings.TrimPrefix(textField(info, "Name"), "/"), target)
		}
		host["NetworkMode"] = "container:" + name
	}
	return spec, volumes, nil
}

// packContainers writes the named containers into out. The file appears
// only when complete; the images committed on the way are untagged again.
func packContainers(c *commandClient, names []string, out string, log io.Writer) (err error) {
	infos := make([]map[string]any, 0, len(names))
	ids := map[string]string{}
	for _, name := range names {
		var info map[string]any
		if err := c.request("GET", "/containers/"+url.PathEscape(name)+"/json", nil, &info); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		id := textField(info, "Id")
		if _, dup := ids[id]; dup {
			continue
		}
		ids[id] = strings.TrimPrefix(textField(info, "Name"), "/")
		infos = append(infos, info)
	}
	manifest := packManifest{Format: packFormat, Version: packVersion, Created: time.Now().UTC()}
	var committed []string
	defer func() {
		for _, ref := range committed {
			_ = c.request("DELETE", "/images/"+ref, nil, nil)
		}
	}()
	stamp := time.Now().Format("20060102150405")
	seenVolume, seenNetwork := map[string]bool{}, map[string]bool{}
	var volumeNames, networkNames []string
	for _, info := range infos {
		name := strings.TrimPrefix(textField(info, "Name"), "/")
		repo := packRepository(name)
		fmt.Fprintf(log, "committing %s\n", name)
		q := url.Values{"container": {textField(info, "Id")}, "repo": {repo}, "tag": {stamp}}
		// A large container takes longer to commit than a request's deadline.
		answer, err := c.stream("POST", "/commit?"+q.Encode(), "", nil, -1)
		if err != nil {
			return fmt.Errorf("commit %s: %w", name, err)
		}
		_, _ = io.Copy(io.Discard, answer)
		answer.Close()
		image := repo + ":" + stamp
		committed = append(committed, image)
		spec, volumes, err := packSpec(info, image, ids)
		if err != nil {
			return err
		}
		running, _ := mapField(info, "State")["Running"].(bool)
		manifest.Containers = append(manifest.Containers, packContainer{
			Name: name, Image: image, SourceImage: textField(mapField(info, "Config"), "Image"), Running: running, Spec: spec,
		})
		for _, v := range volumes {
			if !seenVolume[v] {
				seenVolume[v] = true
				volumeNames = append(volumeNames, v)
			}
		}
		for network := range mapField(mapField(info, "NetworkSettings"), "Networks") {
			if !seenNetwork[network] && !slices.Contains([]string{"bridge", "host", "none", "default"}, network) {
				seenNetwork[network] = true
				networkNames = append(networkNames, network)
			}
		}
	}
	slices.Sort(networkNames)
	for _, name := range networkNames {
		var n struct {
			Name, Driver string
			Labels       map[string]string
			Internal     bool
		}
		if err := c.request("GET", "/networks/"+url.PathEscape(name), nil, &n); err != nil {
			return fmt.Errorf("network %s: %w", name, err)
		}
		manifest.Networks = append(manifest.Networks, packNetwork{Name: name, Driver: n.Driver, Labels: n.Labels, Internal: n.Internal})
	}
	var dataBinds []string
	for _, name := range volumeNames {
		var v struct {
			Driver          string
			Labels, Options map[string]string
		}
		if err := c.request("GET", "/volumes/"+url.PathEscape(name), nil, &v); err != nil {
			return fmt.Errorf("volume %s: %w", name, err)
		}
		vol := packVolume{Name: name, Driver: v.Driver, Labels: v.Labels, Options: v.Options}
		// A volume backed by a host directory or another driver keeps its
		// files there; the package only carries its definition.
		if (v.Driver == "" || v.Driver == "local") && !strings.Contains(v.Options["o"], "bind") && v.Options["type"] == "" {
			index := strconv.Itoa(len(dataBinds))
			vol.Data = "volumes/" + index + ".tar"
			dataBinds = append(dataBinds, name+":"+packHelperRoot+"/"+index+":ro")
		}
		manifest.Volumes = append(manifest.Volumes, vol)
	}

	dir := filepath.Dir(out)
	partial, err := os.CreateTemp(dir, "."+filepath.Base(out)+".*.partial")
	if err != nil {
		return err
	}
	defer func() {
		partial.Close()
		if err != nil {
			os.Remove(partial.Name())
		}
	}()
	buffered := bufio.NewWriterSize(partial, 1<<20)
	zw, _ := gzip.NewWriterLevel(buffered, gzip.BestSpeed)
	tw := tar.NewWriter(zw)
	raw, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	if err := writePackEntry(tw, "manifest.json", strings.NewReader(string(raw)), int64(len(raw))); err != nil {
		return err
	}
	fmt.Fprintf(log, "saving %d images\n", len(committed))
	q := url.Values{"names": committed}
	if err := spoolEntry(c, tw, dir, "images.tar", "/images/get?"+q.Encode()); err != nil {
		return fmt.Errorf("save images: %w", err)
	}
	if len(dataBinds) > 0 {
		helper, err := startPackHelper(c, dataBinds, log)
		if helper != "" {
			defer removePackHelper(c, helper)
		}
		if err != nil {
			return err
		}
		for _, v := range manifest.Volumes {
			if v.Data == "" {
				continue
			}
			fmt.Fprintf(log, "copying volume %s\n", v.Name)
			index := strings.TrimSuffix(strings.TrimPrefix(v.Data, "volumes/"), ".tar")
			path := "/containers/" + url.PathEscape(helper) + "/archive?" + url.Values{"path": {packHelperRoot + "/" + index}}.Encode()
			if err := spoolEntry(c, tw, dir, v.Data, path); err != nil {
				return fmt.Errorf("volume %s: %w", v.Name, err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	if err := zw.Close(); err != nil {
		return err
	}
	if err := buffered.Flush(); err != nil {
		return err
	}
	if err := partial.Close(); err != nil {
		return err
	}
	// CreateTemp makes the file private; the package is an ordinary file.
	mask := syscall.Umask(0)
	syscall.Umask(mask)
	if err := os.Chmod(partial.Name(), 0o666&^os.FileMode(mask)); err != nil {
		return err
	}
	if err := os.Rename(partial.Name(), out); err != nil {
		return err
	}
	if info, err := os.Stat(out); err == nil {
		fmt.Fprintf(log, "wrote %s (%d containers, %d volumes, %.1f MB)\n", out, len(manifest.Containers), len(manifest.Volumes), float64(info.Size())/(1<<20))
	}
	return nil
}

// writePackEntry adds one file to the package.
func writePackEntry(tw *tar.Writer, name string, r io.Reader, size int64) error {
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: size, ModTime: time.Now(), Typeflag: tar.TypeReg}); err != nil {
		return err
	}
	_, err := io.CopyN(tw, r, size)
	return err
}

// spoolEntry downloads path into a temporary file, as a tar entry needs its
// size first, then adds it to the package as name.
func spoolEntry(c *commandClient, tw *tar.Writer, dir, name, path string) error {
	body, err := c.stream("GET", path, "", nil, -1)
	if err != nil {
		return err
	}
	defer body.Close()
	tmp, err := os.CreateTemp(dir, ".fabo-pack-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	size, err := io.Copy(tmp, body)
	if err != nil {
		return err
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return err
	}
	return writePackEntry(tw, name, tmp, size)
}

// packHelperImage is a local image with sleep and tar to hold volumes open,
// pulling alpine only when the engine has neither alpine nor busybox.
func packHelperImage(c *commandClient, log io.Writer) (string, error) {
	var images []struct{ RepoTags []string }
	if err := c.request("GET", "/images/json", nil, &images); err != nil {
		return "", err
	}
	for _, prefer := range []string{"busybox:", "alpine:"} {
		for _, img := range images {
			for _, tag := range img.RepoTags {
				if strings.HasPrefix(familiarName(tag), prefer) {
					return tag, nil
				}
			}
		}
	}
	fmt.Fprintln(log, "pulling alpine:latest to copy volume files")
	if err := c.pullImage("alpine:latest"); err != nil {
		return "", fmt.Errorf("pull alpine:latest to copy volume files: %w", err)
	}
	return "alpine:latest", nil
}

// startPackHelper runs a container that only sleeps, with binds mounted, so
// the archive endpoint can read or fill the volumes whatever state their own
// containers are in. It returns the helper's name even when starting fails,
// for the caller to remove.
func startPackHelper(c *commandClient, binds []string, log io.Writer) (string, error) {
	image, err := packHelperImage(c, log)
	if err != nil {
		return "", err
	}
	name := fmt.Sprintf("fabo-pack-%d-%d", os.Getpid(), time.Now().UnixNano())
	body := map[string]any{
		"Image": image, "Entrypoint": []string{"sleep"}, "Cmd": []string{"86400"},
		"Labels":     map[string]string{packHelperLabel: "1"},
		"HostConfig": map[string]any{"Binds": binds, "NetworkMode": "none"},
	}
	if err := c.request("POST", "/containers/create?name="+url.QueryEscape(name), body, nil); err != nil {
		return "", fmt.Errorf("volume helper: %w", err)
	}
	if err := c.request("POST", "/containers/"+url.PathEscape(name)+"/start", nil, nil); err != nil {
		return name, fmt.Errorf("volume helper: %w", err)
	}
	return name, nil
}

func removePackHelper(c *commandClient, name string) {
	_ = c.request("DELETE", "/containers/"+url.PathEscape(name)+"?force=1", nil, nil)
}

// randomVolumeName makes a new anonymous volume name.
func randomVolumeName() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// unpackFile restores a package: images, networks, volumes and stopped
// containers, then the volumes' files. Anything it created is removed again
// when a step fails; images it loaded stay.
func unpackFile(c *commandClient, file string, start bool, log io.Writer) (err error) {
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer f.Close()
	zr, err := gzip.NewReader(bufio.NewReaderSize(f, 1<<20))
	if err != nil {
		return fmt.Errorf("%s is not a Fabo package: %w", file, err)
	}
	tr := tar.NewReader(zr)
	hdr, err := tr.Next()
	if err != nil || hdr.Name != "manifest.json" || hdr.Size > maxManifest {
		return fmt.Errorf("%s is not a Fabo package", file)
	}
	var m packManifest
	if err := json.NewDecoder(io.LimitReader(tr, maxManifest)).Decode(&m); err != nil {
		return fmt.Errorf("%s is not a Fabo package: %w", file, err)
	}
	if m.Format != packFormat {
		return fmt.Errorf("%s is not a Fabo package", file)
	}
	if m.Version > packVersion {
		return fmt.Errorf("%s needs a newer Fabo (package version %d)", file, m.Version)
	}

	// Check every name before changing anything.
	var rows []struct{ Names []string }
	if err := c.request("GET", "/containers/json?all=1", nil, &rows); err != nil {
		return err
	}
	var vols struct{ Volumes []struct{ Name string } }
	if err := c.request("GET", "/volumes", nil, &vols); err != nil {
		return err
	}
	var nets []struct{ Name string }
	if err := c.request("GET", "/networks", nil, &nets); err != nil {
		return err
	}
	var taken []string
	for _, row := range rows {
		for _, n := range row.Names {
			if slices.ContainsFunc(m.Containers, func(pc packContainer) bool { return pc.Name == strings.TrimPrefix(n, "/") }) {
				taken = append(taken, "container "+strings.TrimPrefix(n, "/"))
			}
		}
	}
	renamed := map[string]string{}
	for _, v := range vols.Volumes {
		if !slices.ContainsFunc(m.Volumes, func(pv packVolume) bool { return pv.Name == v.Name }) {
			continue
		}
		// An anonymous volume's name means nothing; give the copy a new one.
		if anonymousVolume(v.Name) {
			renamed[v.Name] = randomVolumeName()
		} else {
			taken = append(taken, "volume "+v.Name)
		}
	}
	if len(taken) > 0 {
		return fmt.Errorf("already in this engine: %s; remove or rename them first", strings.Join(taken, ", "))
	}
	haveNetwork := map[string]bool{}
	for _, n := range nets {
		haveNetwork[n.Name] = true
	}
	volumeName := func(name string) string { return cmpOr(renamed[name], name) }

	var created struct{ containers, volumes, networks []string }
	defer func() {
		if err == nil {
			return
		}
		for _, name := range slices.Backward(created.containers) {
			_ = c.request("DELETE", "/containers/"+url.PathEscape(name)+"?force=1", nil, nil)
		}
		for _, name := range created.volumes {
			_ = c.request("DELETE", "/volumes/"+url.PathEscape(name), nil, nil)
		}
		for _, name := range created.networks {
			_ = c.request("DELETE", "/networks/"+url.PathEscape(name), nil, nil)
		}
	}()

	hdr, err = tr.Next()
	if err != nil || hdr.Name != "images.tar" {
		return fmt.Errorf("%s has no images", file)
	}
	fmt.Fprintf(log, "loading %d images\n", len(m.Containers))
	if err := loadImages(c, tr, hdr.Size); err != nil {
		return fmt.Errorf("load images: %w", err)
	}
	for _, n := range m.Networks {
		if haveNetwork[n.Name] {
			continue
		}
		body := map[string]any{"Name": n.Name, "Driver": cmpOr(n.Driver, "bridge"), "Labels": n.Labels, "Internal": n.Internal}
		if err := c.request("POST", "/networks/create", body, nil); err != nil {
			return fmt.Errorf("network %s: %w", n.Name, err)
		}
		created.networks = append(created.networks, n.Name)
	}
	var dataBinds []string
	data := map[string]string{}
	for _, v := range m.Volumes {
		name := volumeName(v.Name)
		body := map[string]any{"Name": name, "Driver": cmpOr(v.Driver, "local"), "Labels": v.Labels, "DriverOpts": v.Options}
		if err := c.request("POST", "/volumes/create", body, nil); err != nil {
			return fmt.Errorf("volume %s: %w", name, err)
		}
		created.volumes = append(created.volumes, name)
		if v.Data != "" {
			index := strings.TrimSuffix(strings.TrimPrefix(v.Data, "volumes/"), ".tar")
			if _, err := strconv.Atoi(index); err != nil {
				return fmt.Errorf("%s: bad volume entry %q", file, v.Data)
			}
			data[v.Data] = name
			dataBinds = append(dataBinds, name+":"+packHelperRoot+"/"+index)
		}
	}
	// Containers sharing another's network come after it.
	order := slices.Clone(m.Containers)
	slices.SortStableFunc(order, func(a, b packContainer) int {
		shares := func(pc packContainer) bool {
			host, _ := pc.Spec["HostConfig"].(map[string]any)
			return strings.HasPrefix(textField(host, "NetworkMode"), "container:")
		}
		switch {
		case shares(a) == shares(b):
			return 0
		case shares(a):
			return 1
		}
		return -1
	})
	for _, pc := range order {
		for _, missing := range unpackBinds(pc.Spec, volumeName) {
			fmt.Fprintf(log, "%s: %s is not on this machine; mounting nothing there\n", pc.Name, missing)
		}
		if err := c.request("POST", "/containers/create?name="+url.QueryEscape(pc.Name), pc.Spec, nil); err != nil {
			return fmt.Errorf("container %s: %w", pc.Name, err)
		}
		created.containers = append(created.containers, pc.Name)
	}
	if len(dataBinds) > 0 {
		helper, err := startPackHelper(c, dataBinds, log)
		if helper != "" {
			defer removePackHelper(c, helper)
		}
		if err != nil {
			return err
		}
		filled := 0
		for {
			hdr, err := tr.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				return fmt.Errorf("read %s: %w", file, err)
			}
			name, ok := data[hdr.Name]
			if !ok {
				return fmt.Errorf("%s: unexpected entry %q", file, hdr.Name)
			}
			fmt.Fprintf(log, "filling volume %s\n", name)
			q := url.Values{"path": {packHelperRoot}, "copyUIDGID": {"1"}}
			body, err := c.stream("PUT", "/containers/"+url.PathEscape(helper)+"/archive?"+q.Encode(), "application/x-tar", tr, hdr.Size)
			if err != nil {
				return fmt.Errorf("volume %s: %w", name, err)
			}
			_, _ = io.Copy(io.Discard, body)
			body.Close()
			filled++
		}
		if filled != len(data) {
			return fmt.Errorf("%s is missing volume files", file)
		}
	}
	if start {
		for _, pc := range order {
			if !pc.Running {
				continue
			}
			if err := c.request("POST", "/containers/"+url.PathEscape(pc.Name)+"/start", nil, nil); err != nil {
				// The containers exist and are complete; only starting failed.
				fmt.Fprintf(log, "%s: not started: %v\n", pc.Name, err)
			}
		}
	}
	fmt.Fprintf(log, "unpacked %d containers, %d volumes\n", len(m.Containers), len(m.Volumes))
	return nil
}

// unpackBinds points volume binds at the names the volumes got here, and
// drops host binds whose directory or file this machine lacks, which the
// engine would refuse. It returns the dropped host paths.
func unpackBinds(spec map[string]any, volumeName func(string) string) []string {
	host, _ := spec["HostConfig"].(map[string]any)
	if host == nil {
		return nil
	}
	var missing []string
	exists := func(path string) bool {
		_, err := os.Stat(path)
		return err == nil
	}
	if list, ok := host["Binds"].([]any); ok {
		var kept []any
		for _, b := range list {
			s, _ := b.(string)
			source, rest, _ := strings.Cut(s, ":")
			switch {
			case !hostBind(s):
				kept = append(kept, volumeName(source)+":"+rest)
			case exists(source):
				kept = append(kept, s)
			default:
				missing = append(missing, source)
			}
		}
		host["Binds"] = kept
	}
	if list, ok := host["Mounts"].([]any); ok {
		var kept []any
		for _, m := range list {
			mount, _ := m.(map[string]any)
			if textField(mount, "Type") == "bind" && !exists(textField(mount, "Source")) {
				missing = append(missing, textField(mount, "Source"))
				continue
			}
			kept = append(kept, m)
		}
		host["Mounts"] = kept
	}
	return missing
}

// loadImages streams images.tar into the engine and reports an error the
// load's progress stream carries.
func loadImages(c *commandClient, r io.Reader, size int64) error {
	body, err := c.stream("POST", "/images/load?quiet=1", "application/x-tar", r, size)
	if err != nil {
		return err
	}
	defer body.Close()
	dec := json.NewDecoder(body)
	for {
		var step struct {
			Error       string
			ErrorDetail struct{ Message string }
		}
		if err := dec.Decode(&step); err == io.EOF {
			return nil
		} else if err != nil {
			return err
		}
		if msg := cmpOr(step.ErrorDetail.Message, step.Error); msg != "" {
			return errors.New(msg)
		}
	}
}
