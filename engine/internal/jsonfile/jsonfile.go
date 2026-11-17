// Package jsonfile reads and writes the JSON files Fabo shares with docker
// and buildx: unknown keys and numbers survive, and a write never leaves
// half a file.
package jsonfile

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Object is a JSON object whose unknown keys survive a read and a write.
type Object = map[string]any

// Read reads a JSON object. A missing file is an empty object; anything
// that is not an object is an error, so a file this cannot understand is
// never overwritten.
func Read(path string) (Object, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Object{}, nil
	}
	if err != nil {
		return nil, err
	}
	// Numbers stay as written: docker's files carry no floats to round.
	dec := json.NewDecoder(bytes.NewReader(bytes.TrimPrefix(raw, []byte("\xEF\xBB\xBF"))))
	dec.UseNumber()
	var v Object
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if v == nil {
		v = Object{}
	}
	return v, nil
}

// Encode renders v without escaping the characters HTML cares about,
// indented with docker's own tabs when pretty.
func Encode(v any, pretty bool) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if pretty {
		enc.SetIndent("", "\t")
	}
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	if pretty {
		return buf.Bytes(), nil
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

// Write writes v through WriteFile, pretty-printed.
func Write(path string, v any, mode os.FileMode) error {
	raw, err := Encode(v, true)
	if err != nil {
		return err
	}
	return WriteFile(path, raw, mode)
}

// WriteFile replaces path through a temporary file, so a crash never leaves
// half a file. An existing file keeps its permissions (docker's config.json
// can hold registry credentials) and mode applies only to a new one. A
// symlinked file, as dotfile managers make, is written through its link.
func WriteFile(path string, data []byte, mode os.FileMode) error {
	if target, err := filepath.EvalSymlinks(path); err == nil {
		path = target
	}
	if info, err := os.Stat(path); err == nil {
		mode = info.Mode().Perm()
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".fabo-tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// Exists reports whether path is there, following links.
func Exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// IsFile reports whether path is a regular file, following links.
func IsFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}
