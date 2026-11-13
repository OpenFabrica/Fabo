//go:build linux || darwin

package localengine

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"maps"
	"net/netip"
	"net/url"
	"path"
	"slices"
	"strconv"
	"strings"

	"github.com/google/go-containerregistry/pkg/name"
)

type buildOptions struct {
	tags                                 []string
	dockerfile, remote, target, platform string
	network, buildID                     string
	args                                 map[string]*string
	labels                               map[string]string
	hosts, ulimits, cacheFrom            []string
	shmSize                              uint64
	noCache, pull, quiet                 bool
}

func parseBuildOptions(q url.Values) (buildOptions, error) {
	var o buildOptions
	if v := q.Get("version"); v != "" && v != "1" {
		return o, fmt.Errorf("classic builds use API version 1; select the Kiri Buildx builder for sessions")
	}
	for _, raw := range q["t"] {
		if raw == "" {
			continue
		}
		tag, err := name.NewTag(raw)
		if err != nil || strings.Contains(raw, "@") {
			return o, fmt.Errorf("invalid build tag %q", raw)
		}
		o.tags = append(o.tags, shortImageReference(tag.Name()))
	}
	o.dockerfile = q.Get("dockerfile")
	for part := range strings.SplitSeq(o.dockerfile, "/") {
		if part == ".." || strings.ContainsRune(part, 0) {
			return o, fmt.Errorf("the Dockerfile must be within the build context")
		}
	}
	o.dockerfile = path.Clean(strings.TrimLeft(o.dockerfile, "/"))
	if o.dockerfile == "." {
		o.dockerfile = "Dockerfile"
	}
	o.remote, o.target, o.buildID = q.Get("remote"), q.Get("target"), q.Get("buildid")
	o.platform = strings.ToLower(q.Get("platform"))
	if _, err := imagePlatforms([]string{o.platform}); err != nil {
		return o, err
	}
	switch o.network = q.Get("networkmode"); o.network {
	case "", "default", "bridge":
		o.network = ""
	case "host", "none":
	default:
		return o, fmt.Errorf("unsupported build network mode %q", o.network)
	}
	o.noCache, o.pull, o.quiet = flagSet(q, "nocache"), flagSet(q, "pull"), flagSet(q, "q")
	if raw := q.Get("shmsize"); raw != "" {
		n, err := strconv.ParseUint(raw, 10, 64)
		if err != nil {
			return o, fmt.Errorf("invalid build shared memory size")
		}
		o.shmSize = n
	}
	var limits []struct {
		Name       string
		Soft, Hard int64
	}
	for _, value := range []struct {
		key string
		dst any
	}{{"buildargs", &o.args}, {"labels", &o.labels}, {"ulimits", &limits}, {"cachefrom", &o.cacheFrom}} {
		if raw := q.Get(value.key); raw != "" {
			if err := json.Unmarshal([]byte(raw), value.dst); err != nil {
				return o, fmt.Errorf("invalid build %s: %w", value.key, err)
			}
		}
	}
	for _, limit := range limits {
		if limit.Name == "" {
			return o, fmt.Errorf("build ulimit requires a name")
		}
		o.ulimits = append(o.ulimits, fmt.Sprintf("%s=%d:%d", limit.Name, limit.Soft, limit.Hard))
	}
	for _, raw := range q["extrahosts"] {
		for entry := range strings.SplitSeq(raw, ",") {
			entry = strings.TrimSpace(entry)
			if entry == "" {
				continue
			}
			host, ip, ok := strings.Cut(entry, "=")
			if !ok {
				host, ip, ok = strings.Cut(entry, ":")
			}
			if !ok || host == "" {
				return o, fmt.Errorf("invalid extra host %q", entry)
			}
			if ip == "host-gateway" {
				ip = defaultSubnet.Addr().Next().String()
			}
			ip = strings.Trim(ip, "[]")
			if _, err := netip.ParseAddr(ip); err != nil {
				return o, fmt.Errorf("invalid address in extra host %q", entry)
			}
			o.hosts = append(o.hosts, host+"="+ip)
		}
	}
	for _, ref := range o.cacheFrom {
		if _, err := name.ParseReference(ref); err != nil {
			return o, fmt.Errorf("invalid cache image reference %q", ref)
		}
	}
	return o, nil
}

func (o buildOptions) command(dir string) []string {
	args := []string{"buildctl", "build", "--progress", "plain", "--frontend", "dockerfile.v0"}
	opt := func(key, value string) {
		if value != "" {
			args = append(args, "--opt", key+"="+value)
		}
	}
	if o.remote == "" {
		args = append(args, "--local", "context="+dir+"/context",
			"--local", "dockerfile="+path.Join(dir, "context", path.Dir(o.dockerfile)))
		opt("filename", path.Base(o.dockerfile))
	} else {
		opt("context", o.remote)
		opt("filename", o.dockerfile)
	}
	opt("target", o.target)
	opt("platform", o.platform)
	opt("force-network-mode", o.network)
	opt("add-hosts", strings.Join(o.hosts, ","))
	opt("ulimit", strings.Join(o.ulimits, ","))
	if o.shmSize != 0 {
		opt("shm-size", strconv.FormatUint(o.shmSize, 10))
	}
	if o.pull {
		opt("image-resolve-mode", "pull")
	}
	for _, k := range slices.Sorted(maps.Keys(o.args)) {
		if o.args[k] != nil {
			args = append(args, "--opt", "build-arg:"+k+"="+*o.args[k])
		}
	}
	for _, k := range slices.Sorted(maps.Keys(o.labels)) {
		args = append(args, "--opt", "label:"+k+"="+o.labels[k])
	}
	if o.network == "host" {
		args = append(args, "--allow", "network.host")
	}
	if o.noCache {
		args = append(args, "--no-cache")
	}
	for _, ref := range o.cacheFrom {
		args = append(args, "--import-cache", "type=registry,ref="+ref)
	}
	fields := []string{"type=docker", "dest=-"}
	if len(o.tags) != 0 {
		fields = append(fields, "name="+strings.Join(o.tags, ","))
	}
	var output bytes.Buffer
	csvWriter := csv.NewWriter(&output)
	_ = csvWriter.Write(fields)
	csvWriter.Flush()
	return append(args, "--output", strings.TrimSuffix(output.String(), "\n"))
}
