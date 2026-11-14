//go:build linux || darwin

package localengine

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"

	"github.com/OpenFabrica/FabriBay/engine/internal/guestwire"
	"github.com/OpenFabrica/FabriBay/engine/internal/hostagent"
)

type commitConfig struct {
	v1.ConfigFile
	Config map[string]json.RawMessage `json:"config"`
}

func (e *Engine) commitHTTP(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	var overrides map[string]json.RawMessage
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&overrides); err != nil && err != io.EOF {
		writeError(w, fmt.Errorf("%w: invalid image config: %v", errInvalid, err))
		return
	}
	id, err := e.commitContainer(r.Context(), q, overrides)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"Id": id})
}

func (e *Engine) commitContainer(ctx context.Context, q url.Values, overrides map[string]json.RawMessage) (string, error) {
	containerKey := q.Get("container")
	if containerKey == "" {
		return "", fmt.Errorf("%w: expected a container name or ID", errInvalid)
	}
	ref := ""
	if repo := q.Get("repo"); repo != "" {
		tag := q.Get("tag")
		if tag == "" {
			tag = "latest"
		}
		target, err := name.NewTag(repo + ":" + tag)
		if err != nil {
			return "", fmt.Errorf("%w: invalid image reference", errInvalid)
		}
		ref = target.Name()
	}
	if e.state == nil {
		return "", fmt.Errorf("image storage is unavailable")
	}
	c, err := e.lockOperation(containerKey)
	if err != nil {
		return "", err
	}
	defer c.operation.Unlock()
	e.mu.Lock()
	key := c.imageID
	if key == "" {
		key = c.image
	}
	selected, err := e.findImage(key)
	running := c.state == "running"
	pause := running && !c.paused && (!q.Has("pause") || flagSet(q, "pause"))
	e.mu.Unlock()
	if err != nil {
		return "", err
	}
	base := selected.Image
	if !imageUsedBy(base, c) {
		return "", fmt.Errorf("%w: original container image is unavailable", errMissing)
	}
	cfg, err := e.prepareCommitConfig(ctx, base, c, overrides, q["changes"])
	if err != nil {
		return "", err
	}
	layer, err := e.stageCommitLayer(ctx, c, pause)
	if layer.local != "" {
		defer os.Remove(layer.local)
	}
	if err != nil {
		return "", err
	}
	if len(cfg.RootFS.DiffIDs) != len(base.Layers) || layer.size > math.MaxInt64-base.Size {
		return "", fmt.Errorf("invalid base image layers or size")
	}
	diff := v1.Hash{Algorithm: "sha256", Hex: layer.digest}
	cfg.RootFS.DiffIDs = append(cfg.RootFS.DiffIDs, diff)
	cfg.Created = v1.Time{Time: time.Now().UTC()}
	author := q.Get("author")
	if author != "" {
		cfg.Author = author
	}
	cfg.History = append(cfg.History, v1.History{
		Created: cfg.Created, Author: author, Comment: q.Get("comment"), CreatedBy: "docker commit " + c.id[:min(12, len(c.id))],
	})
	raw, err := json.Marshal(cfg)
	if err != nil {
		return "", err
	}
	if len(raw) > maxArchiveJSON {
		return "", fmt.Errorf("%w: committed image config is too large", errInvalid)
	}
	image, _, err := imageFromConfig(raw)
	if err != nil {
		return "", err
	}
	image.Size = base.Size + layer.size
	image.Layers = append(slices.Clone(base.Layers), hostagent.Layer{Digest: diff.String(), MediaType: plainLayer})
	plan := imageImport{
		tags: make(map[string]Image), untagged: make(map[string]Image),
		layers: map[string]archiveFile{layer.digest: layer},
	}
	if ref == "" {
		plan.untagged[image.ID] = image
	} else {
		plan.tags[ref] = image
	}
	if err := e.importImages(ctx, plan, "commit"); err != nil {
		return "", err
	}
	e.mu.Lock()
	e.emitContainer(c, "commit", nil)
	e.mu.Unlock()
	return image.ID, nil
}

func (e *Engine) prepareCommitConfig(ctx context.Context, base Image, c *container, overrides map[string]json.RawMessage, changes []string) (commitConfig, error) {
	raw := base.ConfigBlob
	if len(raw) == 0 {
		export, err := e.openImageExport([]string{base.ID})
		if err != nil {
			return commitConfig{}, err
		}
		defer export.close()
		raw, err = export.config(ctx, base)
		if err != nil {
			return commitConfig{}, err
		}
	}
	cfg := commitConfig{Config: make(map[string]json.RawMessage)}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return commitConfig{}, err
	}
	if cfg.Config == nil {
		cfg.Config = make(map[string]json.RawMessage)
	}
	entrypoint := c.entrypoint
	if entrypoint == nil && slices.Equal(c.run.Argv[:min(len(c.run.Argv), len(base.Entrypoint))], base.Entrypoint) {
		entrypoint = base.Entrypoint
	}
	if len(entrypoint) > len(c.run.Argv) || !slices.Equal(entrypoint, c.run.Argv[:len(entrypoint)]) {
		return commitConfig{}, fmt.Errorf("container entrypoint does not match its command")
	}
	labels := make(map[string]string, len(base.Labels)+len(c.labels))
	maps.Copy(labels, base.Labels)
	maps.Copy(labels, c.labels)
	for key, value := range map[string]any{
		"Entrypoint": entrypoint, "Cmd": c.run.Argv[len(entrypoint):],
		"Env": c.run.Env, "WorkingDir": c.run.Cwd, "User": c.run.User,
		"Healthcheck": c.healthcheck, "Shell": c.shell, "Labels": labels, "ExposedPorts": c.exposedPorts,
		"StopSignal": strconv.FormatUint(uint64(c.effectiveStopSignal()), 10),
	} {
		if err := setCommitField(cfg.Config, key, value); err != nil {
			return commitConfig{}, err
		}
	}
	for key, value := range overrides {
		if string(value) != "null" {
			cfg.Config[key] = value
		}
	}
	for _, change := range changes {
		for line := range strings.Lines(change) {
			if line = strings.TrimSpace(line); line != "" {
				if err := applyCommitChange(cfg.Config, line); err != nil {
					return commitConfig{}, fmt.Errorf("%w: %v", errInvalid, err)
				}
			}
		}
	}
	return cfg, nil
}

func (e *Engine) stageCommitLayer(ctx context.Context, c *container, pause bool) (layer archiveFile, resultErr error) {
	if err := ctx.Err(); err != nil {
		return layer, err
	}
	if pause {
		freeze, cancel := context.WithTimeout(e.ctx, 10*time.Second)
		err := e.setPaused(freeze, c, true)
		cancel()
		if err != nil {
			return layer, err
		}
		defer func() {
			// A canceled client still owns this pause until the guest resumes.
			thaw, cancel := context.WithTimeout(e.ctx, 10*time.Second)
			defer cancel()
			resultErr = errors.Join(resultErr, e.setPaused(thaw, c, false))
		}()
	}
	stream, closeArchive, err := e.openCopyStream(ctx, c, guestwire.CopyCommit)
	if err != nil {
		return layer, err
	}
	defer closeArchive()
	file, err := os.CreateTemp(e.state.dir, ".commit-layer-")
	if err != nil {
		return layer, err
	}
	hash := sha256.New()
	n, err := io.Copy(io.MultiWriter(file, hash), stream)
	return archiveFile{local: file.Name(), size: n, digest: fmt.Sprintf("%x", hash.Sum(nil))}, errors.Join(err, file.Close())
}
