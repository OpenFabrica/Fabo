//go:build linux || darwin

package localengine

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
)

type buildPruneResult struct {
	CachesDeleted  []string
	SpaceReclaimed uint64
}

func (e *Engine) pruneBuildHTTP(w http.ResponseWriter, r *http.Request) {
	argv, err := buildPruneArgs(r.URL.Query())
	if err != nil {
		writeError(w, err)
		return
	}
	result := buildPruneResult{CachesDeleted: []string{}}
	release := e.holdBuilder()
	defer release()
	c, err := e.readyBuilder(r.Context(), false)
	if errors.Is(err, errMissing) {
		writeJSON(w, http.StatusOK, result)
		return
	}
	if err != nil {
		writeError(w, err)
		return
	}

	var out, stderr bytes.Buffer
	if err := e.runBuilderCommand(r.Context(), c, nil, argv, &out, &stderr); err != nil {
		details := strings.TrimSpace(stderr.String())
		if details != "" {
			writeError(w, fmt.Errorf("prune BuildKit cache: %w: %s", err, details))
		} else {
			writeError(w, fmt.Errorf("prune BuildKit cache: %w", err))
		}
		return
	}
	for line := range bytes.SplitSeq(out.Bytes(), []byte{'\n'}) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		var record struct {
			ID   string
			Size uint64
		}
		if err := json.Unmarshal(line, &record); err != nil || record.ID == "" {
			writeError(w, fmt.Errorf("invalid BuildKit prune result"))
			return
		}
		result.CachesDeleted = append(result.CachesDeleted, record.ID)
		result.SpaceReclaimed += record.Size
	}
	writeJSON(w, http.StatusOK, result)
}

func buildPruneArgs(q url.Values) ([]string, error) {
	// Docker forwards resource labels to the cache endpoint during system
	// prune. The Docker builder accepts them without filtering cache records.
	filters, err := parseFilters(q.Get("filters"), "until", "unused-for", "label", "label!", "id", "parent", "type", "description", "inuse", "shared", "private")
	if err != nil {
		return nil, err
	}
	argv := []string{"buildctl", "prune", "--format", "{{json .}}"}
	if all, _ := strconv.ParseBool(q.Get("all")); all {
		argv = append(argv, "--all")
	}
	// Docker passes byte counts. BuildKit's legacy storage flags use MB.
	for _, option := range []struct{ query, flag string }{
		{"keep-storage", "--keep-storage"}, {"max-used-space", "--keep-storage"},
		{"reserved-space", "--keep-storage-min"}, {"min-free-space", "--free-storage"},
	} {
		if size, err := strconv.ParseUint(q.Get(option.query), 10, 64); err == nil && size > 0 && !slices.Contains(argv, option.flag) {
			argv = append(argv, option.flag, strconv.FormatFloat(float64(size)/1e6, 'f', -1, 64))
		}
	}
	if len(filters["until"]) != 0 && len(filters["unused-for"]) != 0 {
		return nil, fmt.Errorf("%w: until and unused-for cannot be combined", errInvalid)
	}
	values := filters["until"]
	if len(values) == 0 {
		values = filters["unused-for"]
	}
	now := time.Now()
	cutoff, err := pruneCutoff(values, now)
	if err != nil {
		return nil, err
	}
	if !cutoff.IsZero() && cutoff.Before(now) {
		argv = append(argv, "--keep-duration", now.Sub(cutoff).String())
	}
	for _, filter := range []struct{ key, expr string }{
		{"id", "id~="}, {"parent", "parents=="}, {"type", "type=="}, {"description", "description=="},
	} {
		for _, value := range filters[filter.key] {
			argv = append(argv, "--filter", filter.expr+value)
		}
	}
	for _, key := range []string{"inuse", "shared", "private"} {
		if slices.ContainsFunc(filters[key], func(value string) bool {
			on, _ := strconv.ParseBool(value)
			return value == "" || on
		}) {
			argv = append(argv, "--filter", key)
		}
	}
	return argv, nil
}
