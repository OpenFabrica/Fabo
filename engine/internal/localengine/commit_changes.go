//go:build linux || darwin

package localengine

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode"
)

func setCommitField(cfg map[string]json.RawMessage, key string, value any) error {
	raw, err := json.Marshal(value)
	if err == nil {
		cfg[key] = raw
	}
	return err
}

func applyCommitChange(cfg map[string]json.RawMessage, line string) error {
	cut := strings.IndexFunc(line, unicode.IsSpace)
	if cut < 0 {
		return fmt.Errorf("invalid commit change %q", line)
	}
	instruction, arg := strings.ToUpper(line[:cut]), strings.TrimSpace(line[cut:])
	if arg == "" {
		return fmt.Errorf("missing value for %s", instruction)
	}
	switch instruction {
	case "CMD", "ENTRYPOINT":
		key := "Cmd"
		if instruction == "ENTRYPOINT" {
			key = "Entrypoint"
		}
		var words []string
		if strings.HasPrefix(arg, "[") {
			if err := json.Unmarshal([]byte(arg), &words); err != nil {
				return err
			}
		} else {
			words = []string{"/bin/sh", "-c", arg}
		}
		return setCommitField(cfg, key, words)
	case "USER":
		return setCommitField(cfg, "User", arg)
	case "WORKDIR":
		return setCommitField(cfg, "WorkingDir", arg)
	case "ENV", "LABEL":
		key, value, ok := strings.Cut(arg, "=")
		if !ok && instruction == "ENV" {
			if cut := strings.IndexFunc(arg, unicode.IsSpace); cut >= 0 {
				key, value, ok = arg[:cut], arg[cut:], true
			}
		}
		if !ok || strings.TrimSpace(key) == "" {
			return fmt.Errorf("expected a key and value for %s", instruction)
		}
		key, value = strings.TrimSpace(key), strings.Trim(strings.TrimSpace(value), "\"")
		if instruction == "ENV" {
			var env []string
			if raw := cfg["Env"]; raw != nil {
				if err := json.Unmarshal(raw, &env); err != nil {
					return err
				}
			}
			return setCommitField(cfg, "Env", mergeEnv(env, []string{key + "=" + value}))
		}
		var labels map[string]string
		if raw := cfg["Labels"]; raw != nil {
			if err := json.Unmarshal(raw, &labels); err != nil {
				return err
			}
		}
		if labels == nil {
			labels = make(map[string]string)
		}
		labels[key] = value
		return setCommitField(cfg, "Labels", labels)
	case "EXPOSE", "VOLUME":
		key, values := "ExposedPorts", strings.Fields(arg)
		if instruction == "VOLUME" {
			key = "Volumes"
			if strings.HasPrefix(arg, "[") {
				if err := json.Unmarshal([]byte(arg), &values); err != nil {
					return err
				}
			}
		}
		var entries map[string]struct{}
		if raw := cfg[key]; raw != nil {
			if err := json.Unmarshal(raw, &entries); err != nil {
				return err
			}
		}
		if entries == nil {
			entries = make(map[string]struct{})
		}
		for _, value := range values {
			if instruction == "EXPOSE" && !strings.Contains(value, "/") {
				value += "/tcp"
			}
			entries[value] = struct{}{}
		}
		return setCommitField(cfg, key, entries)
	default:
		return fmt.Errorf("unsupported commit change %q", instruction)
	}
}
