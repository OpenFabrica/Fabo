//go:build linux || darwin

package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// checkStore refuses to start beside a populated Kiri (Rust) store that has
// not been imported. The Go engine keeps separate state, and migrate-rust
// needs that state directory to be absent, so starting first would strand
// the existing containers, images and volumes.
func checkStore(state, legacy string) error {
	switch os.Getenv("KIRI_ENGINE") {
	case "go":
		return nil
	case "rust":
		return errors.New("KIRI_ENGINE=rust: this build has no Rust engine")
	}
	for _, marker := range []string{"data.img", "rust-import.json"} {
		if info, err := os.Stat(filepath.Join(state, marker)); err == nil && info.Mode().IsRegular() {
			return nil
		}
	}
	empty, err := emptyDir(legacy)
	if err != nil {
		return err
	}
	if !empty {
		return fmt.Errorf("%s holds a Kiri (Rust) store: import it with `kiri migrate-rust`, or set KIRI_ENGINE=go to start with an empty store", legacy)
	}
	return nil
}

// emptyDir reports whether dir is missing or has no entries.
func emptyDir(dir string) (bool, error) {
	f, err := os.Open(dir)
	if errors.Is(err, os.ErrNotExist) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	defer f.Close()
	if _, err := f.Readdirnames(1); errors.Is(err, io.EOF) {
		return true, nil
	} else if err != nil {
		return false, err
	}
	return false, nil
}
