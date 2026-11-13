//go:build linux || darwin

package localengine

import (
	"encoding/json"
	"fmt"
	"maps"
	"path"
	"slices"
	"strconv"
	"strings"

	"github.com/libracoder/kiri/engine/internal/guestwire"
)

type bindMount struct {
	spec   guestwire.BindMount
	create bool
}

func parseMounts(host map[string]json.RawMessage, windows bool) ([]bindMount, []volumeMount, []guestwire.TmpfsMount, bool, error) {
	var rawBinds []string
	var rawTmpfs map[string]string
	var structured []struct {
		Type, Source, Target string
		ReadOnly             bool
		BindOptions          json.RawMessage
		TmpfsOptions         struct {
			SizeBytes *int64
			Mode      *uint32
			Options   [][]string
		}
		VolumeOptions struct {
			NoCopy       bool
			Labels       map[string]string
			DriverConfig struct {
				Name    string
				Options map[string]string
			}
			Subpath string
		}
	}
	var readonly bool
	for key, dst := range map[string]any{"Binds": &rawBinds, "Mounts": &structured, "Tmpfs": &rawTmpfs, "ReadonlyRootfs": &readonly} {
		if raw, ok := host[key]; ok {
			if err := json.Unmarshal(raw, dst); err != nil {
				return nil, nil, nil, false, fmt.Errorf("%w: invalid %s", errInvalid, key)
			}
			delete(host, key)
		}
	}
	var binds []bindMount
	var volumes []volumeMount
	var tmpfs []guestwire.TmpfsMount
	add := func(m guestwire.BindMount, create bool) error {
		if windows && strings.EqualFold(strings.ReplaceAll(m.Source, `\`, "/"), "//./pipe/docker_engine") {
			m.Source = "/var/run/docker.sock"
		}
		if _, err := m.Encode(); err != nil {
			return fmt.Errorf("%w: %v", errInvalid, err)
		}
		binds = append(binds, bindMount{spec: m, create: create})
		return nil
	}
	addVolume := func(name, target string, ro, noCopy bool, labels, options map[string]string) error {
		if name != "" && !guestwire.ValidVolumeName(name) {
			return fmt.Errorf("%w: invalid volume name %q", errInvalid, name)
		}
		if _, err := (guestwire.BindMount{Source: "/volume", Target: target, ReadOnly: ro}).Encode(); err != nil {
			return fmt.Errorf("%w: %v", errInvalid, err)
		}
		if _, err := localVolumeMount(options); err != nil {
			return err
		}
		volumes = append(volumes, volumeMount{name: name, target: target, readOnly: ro, noCopy: noCopy, labels: labels, options: options})
		return nil
	}
	for _, raw := range rawBinds {
		parts := strings.Split(raw, ":")
		if len(parts) == 1 {
			if err := addVolume("", parts[0], false, false, nil, nil); err != nil {
				return nil, nil, nil, false, err
			}
			continue
		}
		if len(parts) > 3 {
			return nil, nil, nil, false, fmt.Errorf("%w: invalid bind mount", errInvalid)
		}
		ro, noCopy := false, false
		isVolume := !strings.HasPrefix(parts[0], "/")
		if windows && strings.EqualFold(strings.ReplaceAll(parts[0], `\`, "/"), "//./pipe/docker_engine") {
			isVolume = false
		}
		if len(parts) == 3 {
			for _, option := range strings.Split(parts[2], ",") {
				switch {
				case option == "ro":
					ro = true
				case option == "rw":
					ro = false
				case option == "nocopy" && isVolume:
					noCopy = true
				default:
					return nil, nil, nil, false, fmt.Errorf("%w: unsupported mount option %q", errInvalid, option)
				}
			}
		}
		if isVolume {
			if err := addVolume(parts[0], parts[1], ro, noCopy, nil, nil); err != nil {
				return nil, nil, nil, false, err
			}
			continue
		}
		if err := add(guestwire.BindMount{Source: parts[0], Target: parts[1], ReadOnly: ro}, true); err != nil {
			return nil, nil, nil, false, err
		}
	}
	for _, m := range structured {
		if m.Type == "tmpfs" {
			var options []string
			if m.Source != "" {
				return nil, nil, nil, false, fmt.Errorf("%w: tmpfs mounts have no source", errInvalid)
			}
			if size := m.TmpfsOptions.SizeBytes; size != nil {
				if *size < 0 {
					return nil, nil, nil, false, fmt.Errorf("%w: invalid tmpfs size", errInvalid)
				}
				options = append(options, "size="+strconv.FormatInt(*size, 10))
			}
			if mode := m.TmpfsOptions.Mode; mode != nil {
				if *mode > 07777 {
					return nil, nil, nil, false, fmt.Errorf("%w: invalid tmpfs mode", errInvalid)
				}
				options = append(options, "mode="+strconv.FormatUint(uint64(*mode), 8))
			}
			for _, option := range m.TmpfsOptions.Options {
				if len(option) == 0 || len(option) > 2 {
					return nil, nil, nil, false, fmt.Errorf("%w: invalid tmpfs option", errInvalid)
				}
				options = append(options, strings.Join(option, "="))
			}
			if m.ReadOnly {
				options = append(options, "ro")
			}
			tmpfs = append(tmpfs, guestwire.TmpfsMount{Target: m.Target, Options: options})
			continue
		}
		if m.Type == "volume" {
			driver := m.VolumeOptions.DriverConfig
			if m.VolumeOptions.Subpath != "" || (driver.Name != "" && driver.Name != "local") {
				return nil, nil, nil, false, fmt.Errorf("%w: unsupported volume mount options", errInvalid)
			}
			if err := addVolume(m.Source, m.Target, m.ReadOnly, m.VolumeOptions.NoCopy, m.VolumeOptions.Labels, driver.Options); err != nil {
				return nil, nil, nil, false, err
			}
			continue
		}
		opts := string(m.BindOptions)
		if m.Type != "bind" || (opts != "" && opts != "null" && opts != "{}") {
			return nil, nil, nil, false, fmt.Errorf("%w: only bind mounts with default propagation are supported", errInvalid)
		}
		if err := add(guestwire.BindMount{Source: m.Source, Target: m.Target, ReadOnly: m.ReadOnly}, false); err != nil {
			return nil, nil, nil, false, err
		}
	}
	for _, target := range slices.Sorted(maps.Keys(rawTmpfs)) {
		var options []string
		for opt := range strings.SplitSeq(rawTmpfs[target], ",") {
			if opt != "" && opt != "tmpfs" {
				options = append(options, opt)
			}
		}
		tmpfs = append(tmpfs, guestwire.TmpfsMount{Target: target, Options: options})
	}
	if len(tmpfs) != 0 {
		targets := make(map[string]bool)
		for _, b := range binds {
			targets[path.Clean(b.spec.Target)] = true
		}
		for _, v := range volumes {
			targets[path.Clean(v.target)] = true
		}
		for i, mount := range tmpfs {
			if _, err := mount.Encode(); err != nil {
				return nil, nil, nil, false, fmt.Errorf("%w: %v", errInvalid, err)
			}
			target := path.Clean(mount.Target)
			if targets[target] {
				return nil, nil, nil, false, fmt.Errorf("%w: duplicate mount target %q", errInvalid, target)
			}
			targets[target], tmpfs[i].Target = true, target
		}
	}
	return binds, volumes, tmpfs, readonly, nil
}

// declaredVolumes adds anonymous volumes declared by the container or its
// image. Explicit bind and volume mounts take precedence for their targets.
func declaredVolumes(cfg Config, image Image) ([]volumeMount, error) {
	var metadata struct {
		Config struct {
			Volumes map[string]struct{}
		}
	}
	if len(image.ConfigBlob) != 0 {
		if err := json.Unmarshal(image.ConfigBlob, &metadata); err != nil {
			return nil, fmt.Errorf("read image volume declarations: %w", err)
		}
	}
	targets := metadata.Config.Volumes
	if targets == nil {
		targets = make(map[string]struct{})
	}
	for target := range cfg.Volumes {
		targets[target] = struct{}{}
	}
	for _, bind := range cfg.mounts {
		delete(targets, bind.spec.Target)
	}
	for _, mount := range cfg.volumes {
		delete(targets, mount.target)
	}
	for _, mount := range cfg.tmpfs {
		for target := range targets {
			if path.Clean(target) == mount.Target {
				delete(targets, target)
			}
		}
	}
	volumes := slices.Clone(cfg.volumes)
	for _, target := range slices.Sorted(maps.Keys(targets)) {
		if _, err := (guestwire.BindMount{Source: "/volume", Target: target}).Encode(); err != nil {
			return nil, fmt.Errorf("%w: %v", errInvalid, err)
		}
		volumes = append(volumes, volumeMount{target: target})
	}
	return volumes, nil
}
