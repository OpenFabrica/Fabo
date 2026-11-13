//go:build linux || darwin

package localengine

import (
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/libracoder/kiri/engine/internal/hostagent"
)

// The caller holds pulls and imageOps so no new download or container
// can publish a reference between this snapshot and reclamation.
func (e *Engine) collectUnusedLayers() (int64, error) {
	keep := map[string]bool{}
	add := func(layers []hostagent.Layer) {
		for _, layer := range layers {
			digest := strings.ToLower(strings.TrimPrefix(layer.Digest, "sha256:"))
			if validBlobName(digest) {
				keep[digest] = true
			}
		}
	}
	e.mu.Lock()
	for _, image := range e.images {
		add(image.Layers)
	}
	for _, image := range e.imageIndex.Dangling {
		add(image.Layers)
	}
	for _, c := range e.containers {
		add(c.blobs)
	}
	e.mu.Unlock()
	var reclaimed int64
	var err error
	if e.state != nil {
		reclaimed, err = pruneHostBlobs(filepath.Join(e.state.dir, "blobs"), keep)
	}
	if e.session != nil {
		err = errors.Join(err, e.session.PruneLayers(e.ctx, slices.Sorted(maps.Keys(keep))))
	}
	if err != nil {
		return reclaimed, fmt.Errorf("image index updated but layer reclamation was incomplete: %w", err)
	}
	return reclaimed, nil
}

func validBlobName(name string) bool {
	if len(name) != 64 || strings.ToLower(name) != name {
		return false
	}
	_, err := hex.DecodeString(name)
	return err == nil
}

func pruneHostBlobs(dir string, keep map[string]bool) (int64, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	var reclaimed int64
	for _, entry := range entries {
		if !validBlobName(entry.Name()) || keep[entry.Name()] || !entry.Type().IsRegular() {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return reclaimed, err
		}
		if err := os.Remove(filepath.Join(dir, entry.Name())); err != nil {
			return reclaimed, err
		}
		reclaimed += info.Size()
	}
	return reclaimed, nil
}
