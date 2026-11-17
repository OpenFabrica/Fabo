//go:build linux || darwin

package main

import (
	"os"

	"github.com/OpenFabrica/Fabo/engine/internal/jsonfile"
)

// The launcher's JSON files go through internal/jsonfile, which the
// build-with code shares with fabo-win on Windows.

// object is a JSON object whose unknown keys survive a read and a write.
type object = jsonfile.Object

// readJSON reads a JSON object; see jsonfile.Read.
func readJSON(path string) (object, error) { return jsonfile.Read(path) }

// encodeJSON renders v; see jsonfile.Encode.
func encodeJSON(v any, pretty bool) ([]byte, error) { return jsonfile.Encode(v, pretty) }

// writeJSON writes v pretty-printed; see jsonfile.Write.
func writeJSON(path string, v any, mode os.FileMode) error { return jsonfile.Write(path, v, mode) }

// writeFile replaces path atomically; see jsonfile.WriteFile.
func writeFile(path string, data []byte, mode os.FileMode) error {
	return jsonfile.WriteFile(path, data, mode)
}

// exists reports whether path is there, following links.
func exists(path string) bool { return jsonfile.Exists(path) }

// isFile reports whether path is a regular file, following links.
func isFile(path string) bool { return jsonfile.IsFile(path) }
