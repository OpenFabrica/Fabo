//go:build linux || darwin

package localengine

import (
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

func parseFilters(raw string, allowed ...string) (map[string][]string, error) {
	var fields map[string]json.RawMessage
	if raw != "" {
		if err := json.Unmarshal([]byte(raw), &fields); err != nil {
			return nil, fmt.Errorf("%w: invalid filters", errInvalid)
		}
	}
	filters := make(map[string][]string, len(fields))
	for key, field := range fields {
		if !slices.Contains(allowed, key) {
			return nil, fmt.Errorf("%w: unsupported filter %q", errInvalid, key)
		}
		var values []string
		if err := json.Unmarshal(field, &values); err != nil {
			var single string
			if json.Unmarshal(field, &single) == nil {
				filters[key] = []string{single}
				continue
			}
			var legacy map[string]bool
			if err := json.Unmarshal(field, &legacy); err != nil {
				return nil, fmt.Errorf("%w: invalid filter %q", errInvalid, key)
			}
			for value, enabled := range legacy {
				if enabled {
					values = append(values, value)
				}
			}
		}
		filters[key] = values
	}
	return filters, nil
}

func matchesLabels(labels map[string]string, values []string) bool {
	for _, value := range values {
		if !hasLabel(labels, value) {
			return false
		}
	}
	return true
}

func hasLabel(labels map[string]string, filter string) bool {
	label, wanted, exact := strings.Cut(filter, "=")
	actual, ok := labels[label]
	return ok && (!exact || actual == wanted)
}

func matchesContainerName(name, pattern string) bool {
	pattern, start := strings.CutPrefix(pattern, "^")
	pattern, end := strings.CutSuffix(pattern, "$")
	pattern = strings.TrimLeft(pattern, "/")
	switch {
	case start && end:
		return name == pattern
	case start:
		return strings.HasPrefix(name, pattern)
	case end:
		return strings.HasSuffix(name, pattern)
	default:
		return strings.Contains(name, pattern)
	}
}

func (e *Engine) matchesContainer(c *container, filters map[string][]string) bool {
	return matchesLabels(c.labels, filters["label"]) &&
		!slices.ContainsFunc(filters["label!"], func(value string) bool { return hasLabel(c.labels, value) }) &&
		matchAny(filters["name"], func(value string) bool { return matchesContainerName(c.name, value) }) &&
		matchAny(filters["health"], func(value string) bool { return c.healthStatus() == value }) &&
		matchAny(filters["status"], func(value string) bool { return c.dockerState() == value }) &&
		matchAny(filters["id"], func(value string) bool { return strings.HasPrefix(c.id, value) }) &&
		matchAny(filters["exited"], func(value string) bool {
			code, err := strconv.ParseInt(value, 10, 32)
			return c.state == "exited" && err == nil && c.exit == int32(code)
		}) &&
		matchAny(filters["ancestor"], func(value string) bool {
			prefix := strings.TrimPrefix(value, "sha256:")
			if value == c.image || (prefix != "" && strings.HasPrefix(strings.TrimPrefix(c.imageID, "sha256:"), prefix)) {
				return true
			}
			if _, image, found := e.imageReference(value); found && image.ID == c.imageID {
				return true
			}
			repo, _, _ := strings.Cut(c.image, "@")
			if i := strings.LastIndexByte(repo, ':'); i > strings.LastIndexByte(repo, '/') {
				repo = repo[:i]
			}
			return imageReferencesMatch([]string{c.image, repo}, []string{value})
		}) &&
		matchAny(filters["network"], func(value string) bool {
			return c.networkMode() == value || slices.ContainsFunc(c.endpoints(), func(endpoint containerEndpoint) bool {
				return endpoint.Name == value || strings.HasPrefix(endpoint.ID, value)
			})
		}) &&
		matchAny(filters["volume"], func(value string) bool {
			return slices.Contains(c.volumeNames, value) || slices.ContainsFunc(e.containerMounts(c), func(m map[string]any) bool {
				return m["Source"] == value || m["Destination"] == value
			})
		})
}

func matchAny(values []string, match func(string) bool) bool {
	return len(values) == 0 || slices.ContainsFunc(values, match)
}
