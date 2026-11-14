//go:build linux || darwin

package localengine

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"time"

	"github.com/OpenFabrica/Fabo/engine/internal/guestwire"
)

type volume struct {
	Name, Driver, Mountpoint, Scope, CreatedAt string
	Labels                                     map[string]string
	Options                                    map[string]string
	Anonymous                                  bool `json:"anonymous,omitempty"`
}

type volumeMount struct {
	name, target     string
	readOnly, noCopy bool
	labels           map[string]string
	options          map[string]string
}

func (e *Engine) createVolumeHTTP(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name, Driver       string
		Labels, DriverOpts map[string]string
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
		writeError(w, fmt.Errorf("%w: %v", errInvalid, err))
		return
	}
	if body.Driver != "" && body.Driver != "local" {
		writeError(w, fmt.Errorf("%w: only the local volume driver is supported", errInvalid))
		return
	}
	e.mu.Lock()
	v, err := e.ensureVolume(body.Name, body.Labels, body.DriverOpts)
	e.mu.Unlock()
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, v)
}

func (e *Engine) inspectVolumeHTTP(w http.ResponseWriter, r *http.Request) {
	e.mu.Lock()
	v, err := e.readVolume(r.PathValue("name"))
	e.mu.Unlock()
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (e *Engine) listVolumesHTTP(w http.ResponseWriter, r *http.Request) {
	e.mu.Lock()
	volumes, err := e.listVolumes()
	e.mu.Unlock()
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"Volumes": volumes, "Warnings": []string{}})
}

func (e *Engine) removeVolumeHTTP(w http.ResponseWriter, r *http.Request) {
	err := e.removeVolume(r.Context(), r.PathValue("name"))
	if err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Volume helpers are serialized by e.mu so creation and reference changes
// cannot race removal. The state directory lock excludes other daemons.
func (e *Engine) volumeRoot() (string, error) {
	if e.state == nil {
		return "", fmt.Errorf("volume storage is unavailable")
	}
	return filepath.Join(e.state.dir, "volumes"), nil
}

func (e *Engine) readVolume(name string) (volume, error) {
	if !guestwire.ValidVolumeName(name) {
		return volume{}, fmt.Errorf("%w: invalid volume name %q", errInvalid, name)
	}
	root, err := e.volumeRoot()
	if err != nil {
		return volume{}, err
	}
	raw, err := os.ReadFile(filepath.Join(root, name, "volume.json"))
	if errors.Is(err, os.ErrNotExist) {
		return volume{}, fmt.Errorf("%w: volume %q", errMissing, name)
	}
	if err != nil {
		return volume{}, err
	}
	var v volume
	if err := json.Unmarshal(raw, &v); err != nil {
		return volume{}, err
	}
	if v.Name != name {
		return volume{}, fmt.Errorf("invalid metadata for volume %q", name)
	}
	if e.remoteGuest {
		v.Mountpoint = guestwire.VMVolumePath(name)
		return v, nil
	}
	absolute, err := filepath.Abs(filepath.Join(root, name, "_data"))
	if err != nil {
		return volume{}, err
	}
	v.Mountpoint = absolute
	return v, nil
}

func (e *Engine) ensureVolume(name string, labels, options map[string]string) (volume, error) {
	if _, err := localVolumeMount(options); err != nil {
		return volume{}, err
	}
	if e.closed {
		return volume{}, fmt.Errorf("daemon is shutting down")
	}
	if e.remoteGuest && !e.guestDisk {
		return volume{}, fmt.Errorf("%w: VM named volumes require a data disk", errInvalid)
	}
	anonymous := name == ""
	if anonymous {
		var raw [32]byte
		if _, err := rand.Read(raw[:]); err != nil {
			return volume{}, err
		}
		name = hex.EncodeToString(raw[:])
	}
	if e.volumeRemovals[name] {
		return volume{}, fmt.Errorf("%w: volume %q is being removed", errConflict, name)
	}
	v, err := e.readVolume(name)
	if !errors.Is(err, errMissing) {
		return v, err
	}
	root, err := e.volumeRoot()
	if err != nil {
		return volume{}, err
	}
	if err := os.MkdirAll(root, 0700); err != nil {
		return volume{}, err
	}
	stage, err := os.MkdirTemp(root, ".create-")
	if err != nil {
		return volume{}, err
	}
	defer os.RemoveAll(stage)
	if !e.remoteGuest {
		if err := os.Mkdir(filepath.Join(stage, "_data"), 0755); err != nil {
			return volume{}, err
		}
		if err := os.Chmod(filepath.Join(stage, "_data"), 0755); err != nil {
			return volume{}, err
		}
	}
	v = volume{Name: name, Anonymous: anonymous, Driver: "local", Scope: "local", CreatedAt: time.Now().UTC().Format(time.RFC3339Nano), Labels: labels, Options: options}
	if v.Labels == nil {
		v.Labels = map[string]string{}
	}
	if v.Options == nil {
		v.Options = map[string]string{}
	}
	if err := saveJSON(stage, "volume.json", v); err != nil {
		return volume{}, err
	}
	if err := os.Rename(stage, filepath.Join(root, name)); err != nil {
		return volume{}, err
	}
	e.emitVolume(v, "create")
	return e.readVolume(name)
}

func (e *Engine) listVolumes() ([]volume, error) {
	root, err := e.volumeRoot()
	if err != nil {
		return nil, err
	}
	out := make([]volume, 0)
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if !entry.IsDir() || !guestwire.ValidVolumeName(entry.Name()) {
			continue
		}
		v, err := e.readVolume(entry.Name())
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (e *Engine) removeVolume(ctx context.Context, name string) error {
	e.mu.Lock()
	v, err := e.checkVolumeRemoval(name)
	if err != nil {
		e.mu.Unlock()
		return err
	}
	return e.removeVolumeLocked(ctx, v)
}

// removeVolumeLocked takes ownership of e.mu after checking references.
func (e *Engine) removeVolumeLocked(ctx context.Context, v volume) error {
	name := v.Name
	if !e.remoteGuest && len(v.Options) == 0 {
		defer e.mu.Unlock()
		return e.removeVolumeRecord(v)
	}
	if e.volumeRemovals == nil {
		e.volumeRemovals = make(map[string]bool)
	}
	e.volumeRemovals[name] = true
	e.mu.Unlock()

	// The session reader may deliver a container event before the ACK,
	// so it must be free to take e.mu while this request waits.
	err := e.session.DeleteVolume(ctx, name)

	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.volumeRemovals, name)
	if err != nil {
		return fmt.Errorf("remove volume %q: %w", name, err)
	}
	return e.removeVolumeRecord(v)
}

func (e *Engine) checkVolumeRemoval(name string) (volume, error) {
	if e.closed {
		return volume{}, fmt.Errorf("daemon is shutting down")
	}
	if e.remoteGuest && !e.guestDisk {
		return volume{}, fmt.Errorf("%w: VM named volumes require a data disk", errInvalid)
	}
	if e.volumeRemovals[name] {
		return volume{}, fmt.Errorf("%w: volume %q is being removed", errConflict, name)
	}
	v, err := e.readVolume(name)
	if err != nil {
		return volume{}, err
	}
	for _, c := range e.containers {
		if slices.Contains(c.volumeNames, name) {
			return volume{}, fmt.Errorf("%w: volume %q is in use", errConflict, name)
		}
	}
	return v, nil
}

func (e *Engine) removeVolumeRecord(v volume) error {
	root, err := e.volumeRoot()
	if err != nil {
		return err
	}
	trash, err := os.MkdirTemp(root, ".remove-")
	if err != nil {
		return err
	}
	defer os.Remove(trash)
	data := filepath.Join(trash, v.Name)
	if err := os.Rename(filepath.Join(root, v.Name), data); err != nil {
		return err
	}
	e.emitVolume(v, "destroy")
	return os.RemoveAll(data)
}

func (e *Engine) mountVolumes(spec []volumeMount) ([]string, []string, []string, error) {
	var mounts, names, anonymous []string
	for _, item := range spec {
		v, err := e.ensureVolume(item.name, item.labels, item.options)
		if err != nil {
			return nil, nil, nil, err
		}
		driver, err := localVolumeMount(v.Options)
		if err != nil {
			return nil, nil, nil, err
		}
		encoded, err := (guestwire.BindMount{Source: v.Mountpoint, Target: item.target, ReadOnly: item.readOnly, CopyUp: !item.noCopy, Create: e.remoteGuest, Driver: driver}).Encode()
		if err != nil {
			return nil, nil, nil, fmt.Errorf("%w: %v", errInvalid, err)
		}
		mounts = append(mounts, encoded)
		if !slices.Contains(names, v.Name) {
			names = append(names, v.Name)
		}
		if item.name == "" {
			anonymous = append(anonymous, v.Name)
		}
	}
	return mounts, names, anonymous, nil
}

// Remove only volumes allocated anonymously for this container. Another
// container may already refer to one by name, in which case it is kept.
func (e *Engine) removeAnonymousVolumes(ctx context.Context, names []string) error {
	var failures []error
	for _, name := range names {
		e.mu.Lock()
		v, err := e.checkVolumeRemoval(name)
		if err != nil || !v.Anonymous {
			e.mu.Unlock()
			if err != nil && !errors.Is(err, errMissing) && !errors.Is(err, errConflict) {
				failures = append(failures, err)
			}
			continue
		}
		if err := e.removeVolumeLocked(ctx, v); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

// mountVolumeName identifies a volume by its recorded reference and path.
func (e *Engine) mountVolumeName(c *container, m guestwire.BindMount) string {
	if e.remoteGuest {
		if name, ok := guestwire.VolumeNameAt(m.Source); m.Create && ok {
			return name
		}
		return ""
	}
	if len(c.volumeNames) == 0 {
		return ""
	}
	root, err := e.volumeRoot()
	if err != nil {
		return ""
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return ""
	}
	for _, name := range c.volumeNames {
		if m.Source == filepath.Join(root, name, "_data") {
			return name
		}
	}
	return ""
}
