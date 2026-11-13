//go:build linux || darwin

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Preserve unrelated contexts, comments and line endings. Like upstream,
// accept kubectl's block-list YAML and refuse JSON/flow-list rewrites.
type kubeSection struct {
	key   string
	lines []string
}
type kubeList struct {
	before []string
	after  []string
	items  [][]string
	indent int
}

var kubeLists = []string{"clusters", "contexts", "users"}

func kubeBlank(s string) bool {
	return strings.TrimSpace(s) == "" || strings.HasPrefix(strings.TrimSpace(s), "#")
}
func kubeIndent(s string) int { return len(s) - len(strings.TrimLeft(s, " ")) }
func kubeScalar(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 0 && (s[0] == '\'' || s[0] == '"') {
		if n := strings.IndexByte(s[1:], s[0]); n >= 0 {
			return s[1 : n+1]
		}
	}
	return strings.TrimSpace(strings.SplitN(s, " #", 2)[0])
}
func kubeSections(text string) ([]kubeSection, error) {
	if strings.HasPrefix(strings.TrimSpace(text), "{") {
		return nil, errors.New("a JSON kubeconfig; not changing it")
	}
	out := []kubeSection{{}}
	lines := strings.Split(strings.TrimSuffix(strings.ReplaceAll(text, "\r\n", "\n"), "\n"), "\n")
	seen := map[string]bool{}
	for _, line := range lines {
		key, _, ok := strings.Cut(line, ":")
		if ok && line != "" && !strings.ContainsRune(" \t-#", rune(line[0])) {
			key = strings.TrimSpace(key)
			if seen[key] {
				return nil, fmt.Errorf("duplicate kubeconfig key %s; not changing it", key)
			}
			seen[key] = true
			out = append(out, kubeSection{key: key, lines: []string{line}})
		} else {
			out[len(out)-1].lines = append(out[len(out)-1].lines, line)
		}
	}
	return out, nil
}
func kubeItems(s kubeSection) (kubeList, error) {
	var out kubeList
	_, value, _ := strings.Cut(s.lines[0], ":")
	value = kubeScalar(value)
	if value != "" && value != "[]" && value != "null" && value != "~" {
		return out, fmt.Errorf("%s is not a block list; not changing it", s.key)
	}
	for _, line := range s.lines[1:] {
		if len(out.items) == 0 && (kubeBlank(line) || !strings.HasPrefix(strings.TrimSpace(line), "-")) {
			if !kubeBlank(line) {
				return out, fmt.Errorf("invalid %s list; not changing it", s.key)
			}
			out.before = append(out.before, line)
			continue
		}
		if len(out.items) == 0 {
			out.indent = kubeIndent(line)
		}
		if kubeIndent(line) == out.indent && strings.HasPrefix(strings.TrimSpace(line), "-") {
			out.items = append(out.items, []string{line})
		} else {
			out.items[len(out.items)-1] = append(out.items[len(out.items)-1], line)
		}
	}
	if len(out.items) > 0 {
		last := len(out.items) - 1
		lines := out.items[last]
		at := len(lines)
		for at > 1 && kubeBlank(lines[at-1]) {
			at--
		}
		out.after = lines[at:]
		out.items[last] = lines[:at]
	}
	return out, nil
}
func kubeItemName(item []string, indent int) string {
	rest := item[0][indent+1:]
	col := indent + 1 + kubeIndent(rest)
	if strings.HasPrefix(strings.TrimSpace(rest), "name:") {
		return kubeScalar(strings.TrimPrefix(strings.TrimSpace(rest), "name:"))
	}
	for _, line := range item[1:] {
		if kubeIndent(line) == col && strings.HasPrefix(strings.TrimSpace(line), "name:") {
			return kubeScalar(strings.TrimPrefix(strings.TrimSpace(line), "name:"))
		}
	}
	return ""
}
func kubeFromK3s(raw, name string, port int) (string, error) {
	lines := strings.Split(strings.TrimSuffix(strings.ReplaceAll(raw, "\r\n", "\n"), "\n"), "\n")
	server := false
	for n, line := range lines {
		trimmed := strings.TrimLeft(line, " ")
		pad := line[:len(line)-len(trimmed)]
		switch trimmed {
		case "name: default", "- name: default", "cluster: default", "user: default", "current-context: default":
			lines[n] = pad + strings.ReplaceAll(trimmed, "default", name)
		default:
			if strings.HasPrefix(trimmed, "server: https://127.0.0.1:") {
				lines[n] = fmt.Sprintf("%sserver: https://127.0.0.1:%d", pad, port)
				server = true
			}
		}
	}
	if !server {
		return "", errors.New("could not read the cluster's kubeconfig server")
	}
	out := strings.Join(lines, "\n") + "\n"
	sections, err := kubeSections(out)
	if err != nil {
		return "", err
	}
	for _, key := range kubeLists {
		found := false
		for _, s := range sections {
			if s.key == key {
				list, e := kubeItems(s)
				if e != nil {
					return "", e
				}
				for _, item := range list.items {
					if kubeItemName(item, list.indent) == name {
						found = true
					}
				}
			}
		}
		if !found {
			return "", fmt.Errorf("kubeconfig has no %s entry for %s", key, name)
		}
	}
	return out, nil
}
func mergeKubeconfig(existing, ours, name string, use, remove bool) (string, error) {
	sections, err := kubeSections(existing)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(existing) == "" {
		if remove {
			return existing, nil
		}
		return ours, nil
	}
	incoming := map[string][]string{}
	if !remove {
		fresh, err := kubeSections(ours)
		if err != nil {
			return "", err
		}
		for _, s := range fresh {
			for _, key := range kubeLists {
				if s.key != key {
					continue
				}
				list, err := kubeItems(s)
				if err != nil {
					return "", err
				}
				for _, item := range list.items {
					if kubeItemName(item, list.indent) == name {
						for _, line := range item {
							if len(line) >= list.indent {
								incoming[key] = append(incoming[key], line[list.indent:])
							} else {
								incoming[key] = append(incoming[key], "")
							}
						}
					}
				}
			}
		}
	}
	var out []string
	seen := map[string]bool{}
	current, changed := false, false
	for _, s := range sections {
		if s.key == "current-context" {
			_, v, _ := strings.Cut(s.lines[0], ":")
			if use || remove && kubeScalar(v) == name {
				value := name
				if remove {
					value = `""`
				}
				out = append(out, "current-context: "+value)
				out = append(out, s.lines[1:]...)
				current = true
				changed = true
				continue
			}
		}
		isList := false
		for _, key := range kubeLists {
			if s.key == key {
				isList = true
			}
		}
		if !isList || !remove && incoming[s.key] == nil {
			out = append(out, s.lines...)
			continue
		}
		seen[s.key] = true
		list, err := kubeItems(s)
		if err != nil {
			return "", err
		}
		var body []string
		count := 0
		for _, item := range list.items {
			if kubeItemName(item, list.indent) != name {
				body = append(body, item...)
				count++
			}
		}
		if remove && count == len(list.items) {
			out = append(out, s.lines...)
			continue
		}
		changed = true
		header := s.key + ":"
		comment := ""
		if pos := strings.Index(s.lines[0], " #"); pos >= 0 {
			comment = s.lines[0][pos:]
		}
		if remove && count == 0 {
			header += " null"
		}
		out = append(out, header+comment)
		out = append(out, list.before...)
		if !remove {
			at := len(body)
			for at > 0 && kubeBlank(body[at-1]) {
				at--
			}
			out = append(out, body[:at]...)
			for _, line := range incoming[s.key] {
				if line != "" {
					line = strings.Repeat(" ", list.indent) + line
				}
				out = append(out, line)
			}
			out = append(out, body[at:]...)
		} else {
			out = append(out, body...)
		}
		out = append(out, list.after...)
	}
	if !remove {
		for _, key := range kubeLists {
			if !seen[key] && incoming[key] != nil {
				out = append(out, key+":")
				out = append(out, incoming[key]...)
			}
		}
	}
	if use && !current {
		out = append(out, "current-context: "+name)
	}
	if remove && !changed {
		return existing, nil
	}
	nl := "\n"
	if strings.Contains(existing, "\r\n") {
		nl = "\r\n"
	}
	return strings.Join(out, nl) + nl, nil
}
func (i installation) kubePath() string {
	for _, p := range filepath.SplitList(os.Getenv("KUBECONFIG")) {
		if p != "" {
			return p
		}
	}
	return filepath.Join(i.home, ".kube", "config")
}
func updateKubeconfig(path, ours, name string, use, remove bool) (bool, error) {
	// Follow dotfile symlinks, but do not overwrite a dangling link.
	if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSymlink != 0 {
		var e error
		path, e = filepath.EvalSymlinks(path)
		if e != nil {
			return false, e
		}
	}
	before, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	if errors.Is(err, os.ErrNotExist) && remove {
		return false, nil
	}
	after, err := mergeKubeconfig(string(before), ours, name, use, remove)
	if err != nil {
		return false, fmt.Errorf("%s: %w", path, err)
	}
	if string(before) == after {
		return false, nil
	}
	if before != nil {
		if err := writePrivateFile(path+".bak.kiri", before); err != nil {
			return false, err
		}
	}
	if err := writePrivateFile(path, []byte(after)); err != nil {
		return false, err
	}
	return true, nil
}

// Kubeconfig embeds private keys, including when replacing an existing file.
func writePrivateFile(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".kiri-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err = tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
