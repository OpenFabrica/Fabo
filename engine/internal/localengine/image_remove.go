//go:build linux || darwin

package localengine

import (
	"fmt"
	"maps"
	"net/http"
	"slices"
	"time"
)

type imageDeletion struct {
	Untagged string `json:",omitempty"`
	Deleted  string `json:",omitempty"`
}

func (e *Engine) removeImageHTTP(w http.ResponseWriter, r *http.Request) {
	e.pulls.Lock()
	defer e.pulls.Unlock()
	e.imageOps.Lock()
	defer e.imageOps.Unlock()
	changes, err := e.removeImage(r.PathValue("ref"), flagSet(r.URL.Query(), "force"))
	if err == nil && len(changes) != 0 && changes[len(changes)-1].Deleted != "" {
		_, err = e.collectUnusedLayers()
	}
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, changes)
}

func (e *Engine) removeImage(ref string, force bool) ([]imageDeletion, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return nil, fmt.Errorf("%w: daemon is shutting down", errConflict)
	}
	image, err := e.findImage(ref)
	if err != nil {
		return nil, err
	}
	refs := e.imageRefs(image.ID)
	selected := refs
	if image.ref != "" {
		selected = []string{image.ref}
	} else if len(refs) > 1 && !force {
		return nil, fmt.Errorf("%w: image %s has multiple references", errConflict, image.ID)
	}
	removeAll := len(selected) == len(refs)
	if removeAll && !force {
		for _, c := range e.containers {
			if imageUsedBy(image.Image, c) {
				return nil, fmt.Errorf("%w: image %s is referenced by container %s", errConflict, image.ID, c.name)
			}
		}
	}
	next := e.imageIndex.clone()
	changes := []imageDeletion{}
	for _, ref := range selected {
		e.removeImageRef(&next, ref)
		changes = append(changes, imageDeletion{Untagged: ref})
	}
	if removeAll {
		delete(next.Dangling, image.ID)
		changes = append(changes, imageDeletion{Deleted: image.ID})
	}
	if err := e.commitImages(next); err != nil {
		return nil, err
	}
	for _, ref := range selected {
		e.emitImage(image.Image, "untag", ref)
	}
	if removeAll {
		e.emitImage(image.Image, "delete", image.ID)
	}
	return changes, nil
}

func (e *Engine) pruneImagesHTTP(w http.ResponseWriter, r *http.Request) {
	filters, err := parseFilters(r.URL.Query().Get("filters"), "dangling", "until", "label", "label!")
	if err != nil {
		writeError(w, err)
		return
	}
	dangling, err := pruneBool(filters, "dangling")
	if err != nil {
		writeError(w, err)
		return
	}
	until, err := pruneCutoff(filters["until"], time.Now())
	if err != nil {
		writeError(w, err)
		return
	}
	e.pulls.Lock()
	defer e.pulls.Unlock()
	e.imageOps.Lock()
	defer e.imageOps.Unlock()
	changes, err := e.pruneImages(dangling == nil || *dangling, until, filters)
	var reclaimed int64
	if err == nil {
		reclaimed, err = e.collectUnusedLayers()
	}
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ImagesDeleted": changes, "SpaceReclaimed": reclaimed})
}

func (e *Engine) pruneImages(danglingOnly bool, cutoff time.Time, filters map[string][]string) ([]imageDeletion, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return nil, fmt.Errorf("%w: daemon is shutting down", errConflict)
	}
	next := e.imageIndex.clone()
	changes := []imageDeletion{}
	all := e.allImages()
	for _, id := range slices.Sorted(maps.Keys(all)) {
		image := all[id]
		refs := e.imageRefs(id)
		if danglingOnly && len(refs) > 0 || !cutoff.IsZero() && !time.Unix(image.Created, 0).Before(cutoff) ||
			!pruneLabelMatch(image.Labels, filters) {
			continue
		}
		used := false
		for _, c := range e.containers {
			if imageUsedBy(image, c) {
				used = true
				break
			}
		}
		if used {
			continue
		}
		for _, ref := range refs {
			e.removeImageRef(&next, ref)
			changes = append(changes, imageDeletion{Untagged: ref})
		}
		delete(next.Dangling, id)
		changes = append(changes, imageDeletion{Deleted: id})
	}
	if len(changes) == 0 {
		return changes, nil
	}
	previous := e.images
	if err := e.commitImages(next); err != nil {
		return nil, err
	}
	for _, change := range changes {
		if change.Untagged != "" {
			e.emitImage(previous[change.Untagged], "untag", change.Untagged)
		} else {
			e.emitImage(all[change.Deleted], "delete", change.Deleted)
		}
	}
	return changes, nil
}
