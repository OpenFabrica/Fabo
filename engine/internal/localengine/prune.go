//go:build linux || darwin

package localengine

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/libracoder/kiri/engine/internal/guestwire"
)

func (e *Engine) pruneContainersHTTP(w http.ResponseWriter, r *http.Request) {
	filters, cutoff, err := resourcePruneFilters(r)
	if err != nil {
		writeError(w, err)
		return
	}
	e.mu.Lock()
	var candidates []string
	for id, c := range e.containers {
		if e.prunableContainer(c) && pruneLabelMatch(c.labels, filters) && (cutoff.IsZero() || time.Unix(c.created, 0).Before(cutoff)) {
			candidates = append(candidates, id)
		}
	}
	e.mu.Unlock()
	slices.Sort(candidates)
	deleted := []string{}
	for _, id := range candidates {
		if r.Context().Err() != nil {
			break
		}
		ok, err := e.pruneContainer(r.Context(), id)
		if err != nil {
			writeError(w, err)
			return
		}
		if ok {
			deleted = append(deleted, id)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ContainersDeleted": deleted, "SpaceReclaimed": 0})
}

func (e *Engine) prunableContainer(c *container) bool {
	return !c.removing && c.restartTimer == nil && (c.state == "created" || c.state == "exited") &&
		!(c.labels["dev.kiri.builder"] == "1" && e.managedBuilderName(c.name) != "")
}

func (e *Engine) pruneContainer(ctx context.Context, id string) (bool, error) {
	c, err := e.lockOperation(id)
	if errors.Is(err, errConflict) || errors.Is(err, errMissing) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer c.operation.Unlock()
	e.mu.Lock()
	if !e.prunableContainer(c) {
		e.mu.Unlock()
		return false, nil
	}
	err = e.removeLocked(ctx, c, false)
	return err == nil, err
}

func (e *Engine) pruneNetworksHTTP(w http.ResponseWriter, r *http.Request) {
	filters, cutoff, err := resourcePruneFilters(r)
	if err != nil {
		writeError(w, err)
		return
	}
	e.mu.Lock()
	networks := maps.Clone(e.networks)
	e.mu.Unlock()
	deleted := []string{}
	for _, name := range slices.Sorted(maps.Keys(networks)) {
		n := networks[name]
		if r.Context().Err() != nil {
			break
		}
		if !guestwire.CustomBridge(n.Bridge) || !pruneLabelMatch(n.Labels, filters) || (!cutoff.IsZero() && !n.Created.Before(cutoff)) {
			continue
		}
		if err := e.removeNetwork(r.Context(), n.ID); err != nil {
			if errors.Is(err, errConflict) || errors.Is(err, errMissing) {
				continue
			}
			writeError(w, err)
			return
		}
		deleted = append(deleted, name)
	}
	writeJSON(w, http.StatusOK, map[string]any{"NetworksDeleted": deleted})
}

func (e *Engine) pruneVolumesHTTP(w http.ResponseWriter, r *http.Request) {
	filters, err := parseFilters(r.URL.Query().Get("filters"), "all", "label", "label!")
	if err != nil {
		writeError(w, err)
		return
	}
	all, err := pruneBool(filters, "all")
	if err != nil {
		writeError(w, err)
		return
	}
	e.mu.Lock()
	volumes, err := e.listVolumes()
	e.mu.Unlock()
	if err != nil {
		writeError(w, err)
		return
	}
	deleted := []string{}
	for _, v := range volumes {
		if r.Context().Err() != nil {
			break
		}
		if !(v.Anonymous || all != nil && *all) || !pruneLabelMatch(v.Labels, filters) {
			continue
		}
		ok, err := e.pruneVolume(r.Context(), v)
		if err != nil {
			writeError(w, err)
			return
		}
		if ok {
			deleted = append(deleted, v.Name)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"VolumesDeleted": deleted, "SpaceReclaimed": 0})
}

func (e *Engine) pruneVolume(ctx context.Context, original volume) (bool, error) {
	e.mu.Lock()
	v, err := e.checkVolumeRemoval(original.Name)
	if err != nil {
		e.mu.Unlock()
		if errors.Is(err, errMissing) || errors.Is(err, errConflict) {
			return false, nil
		}
		return false, err
	}
	if v.CreatedAt != original.CreatedAt {
		e.mu.Unlock()
		return false, nil
	}
	err = e.removeVolumeLocked(ctx, v)
	return err == nil, err
}

func resourcePruneFilters(r *http.Request) (map[string][]string, time.Time, error) {
	filters, err := parseFilters(r.URL.Query().Get("filters"), "until", "label", "label!")
	if err != nil {
		return nil, time.Time{}, err
	}
	cutoff, err := pruneCutoff(filters["until"], time.Now())
	return filters, cutoff, err
}

func pruneBool(filters map[string][]string, key string) (*bool, error) {
	values := filters[key]
	if len(values) == 0 {
		return nil, nil
	}
	if len(values) != 1 || (values[0] != "true" && values[0] != "false") {
		return nil, fmt.Errorf("%w: %s must be true or false", errInvalid, key)
	}
	v := values[0] == "true"
	return &v, nil
}

func pruneLabelMatch(labels map[string]string, filters map[string][]string) bool {
	matches := func(value string) bool { return matchesLabels(labels, []string{value}) }
	positive := filters["label"]
	return (len(positive) == 0 || slices.ContainsFunc(positive, matches)) && !slices.ContainsFunc(filters["label!"], matches)
}

func pruneCutoff(values []string, now time.Time) (time.Time, error) {
	if len(values) == 0 {
		return time.Time{}, nil
	}
	if len(values) != 1 {
		return time.Time{}, fmt.Errorf("%w: expected one prune cutoff", errInvalid)
	}
	value := values[0]
	if d, err := time.ParseDuration(value); err == nil {
		return now.Add(-d), nil
	}
	seconds, fraction, fractional := strings.Cut(value, ".")
	if sec, err := strconv.ParseInt(seconds, 10, 64); err == nil {
		if !fractional {
			return time.Unix(sec, 0), nil
		}
		if len(fraction) > 0 && len(fraction) <= 9 && strings.Trim(fraction, "0123456789") == "" {
			nanos, _ := strconv.ParseInt(fraction+strings.Repeat("0", 9-len(fraction)), 10, 64)
			return time.Unix(sec, nanos), nil
		}
	}
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05", "2006-01-02T15:04", "2006-01-02"} {
		if cutoff, err := time.ParseInLocation(layout, value, now.Location()); err == nil {
			return cutoff, nil
		}
	}
	return time.Time{}, fmt.Errorf("%w: invalid prune cutoff %q", errInvalid, strings.TrimSpace(value))
}
