//go:build linux || darwin

package localengine

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/libracoder/kiri/engine/internal/guestwire"
)

// ShareRoots are the canonical host roots in the order exported by VirtioFS.
type ShareRoots []string

func CanonicalShareRoots(paths []string) (ShareRoots, error) {
	var roots ShareRoots
	for _, p := range paths {
		absolute, err := filepath.Abs(p)
		if err != nil {
			return nil, err
		}
		root, err := filepath.EvalSymlinks(absolute)
		if err != nil {
			return nil, err
		}
		info, err := os.Stat(root)
		if err != nil {
			return nil, err
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("VM share %q must be a directory", p)
		}
		if !slices.Contains(roots, root) {
			roots = append(roots, root)
		}
	}
	return roots, nil
}

// shareSource resolves the host path before translating it to the guest.
// Creation for -v is confined to the configured share, including symlinks.
func (roots ShareRoots) source(source string, create bool) (string, error) {
	if len(roots) == 0 {
		return "", fmt.Errorf("%w: VM bind mounts require a configured -share directory", errInvalid)
	}
	resolved, err := filepath.EvalSymlinks(source)
	if errors.Is(err, os.ErrNotExist) && create {
		ancestor := filepath.Clean(source)
		for errors.Is(err, os.ErrNotExist) {
			parent := filepath.Dir(ancestor)
			if parent == ancestor {
				break
			}
			ancestor = parent
			resolved, err = filepath.EvalSymlinks(ancestor)
		}
		if err != nil {
			return "", fmt.Errorf("%w: bind source %q: %v", errInvalid, source, err)
		}
		tail, err := filepath.Rel(ancestor, filepath.Clean(source))
		if err != nil {
			return "", err
		}
		index, relative, err := roots.relative(filepath.Join(resolved, tail))
		if err != nil {
			return "", err
		}
		dir, err := os.OpenRoot(roots[index])
		if err != nil {
			return "", err
		}
		defer dir.Close()
		if err := dir.MkdirAll(relative, 0755); err != nil {
			return "", fmt.Errorf("%w: create bind source %q: %v", errInvalid, source, err)
		}
		resolved, err = filepath.EvalSymlinks(source)
	}
	if err != nil {
		return "", bindSourceError(source, err)
	}
	index, relative, err := roots.relative(resolved)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", fmt.Errorf("%w: bind source %q: %v", errInvalid, source, err)
	}
	if !info.IsDir() && !info.Mode().IsRegular() {
		return "", fmt.Errorf("%w: bind source %q must be a directory or regular file", errInvalid, source)
	}
	mount := guestwire.HostSharePath
	if len(roots) > 1 {
		mount = path.Join(mount, strconv.Itoa(index))
	}
	return path.Join(mount, filepath.ToSlash(relative)), nil
}

func bindSourceError(source string, err error) error {
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%w: bind source path does not exist: %s", errInvalid, source)
	}
	return fmt.Errorf("%w: bind source %q: %v", errInvalid, source, err)
}

func (roots ShareRoots) relative(source string) (int, string, error) {
	for i, root := range roots {
		relative, err := filepath.Rel(root, source)
		if err != nil {
			return 0, "", err
		}
		if relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return i, relative, nil
		}
	}
	return 0, "", fmt.Errorf("%w: bind source %q is outside the VM shares", errInvalid, source)
}

// mapGuestMounts builds a transient Run spec. Persisted records keep the
// original host paths so a changed share cannot silently remap a container.
func mapGuestMounts(roots ShareRoots, mounts []string) ([]string, error) {
	var mapped []string
	for _, value := range mounts {
		if guestwire.IsTmpfsMount(value) {
			mapped = append(mapped, value)
			continue
		}
		mount, err := guestwire.DecodeBindMount(value)
		if err != nil {
			return nil, err
		}
		if mount.Source == guestwire.EngineSocketPath {
			mapped = append(mapped, value)
			continue
		}
		if mount.Create {
			if _, ok := guestwire.VolumeNameAt(mount.Source); !ok {
				return nil, fmt.Errorf("%w: invalid VM volume path %q", errInvalid, mount.Source)
			}
			if mount.Driver != nil && mount.Driver.Bind() {
				mount.Driver.Device, err = roots.source(mount.Driver.Device, false)
				if err != nil {
					return nil, err
				}
				value, err = mount.Encode()
				if err != nil {
					return nil, err
				}
			}
			mapped = append(mapped, value)
			continue
		}
		mount.Source, err = roots.source(mount.Source, false)
		if err != nil {
			return nil, err
		}
		encoded, err := mount.Encode()
		if err != nil {
			return nil, err
		}
		mapped = append(mapped, encoded)
	}
	return mapped, nil
}
