package guestagent

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// prune runs on the control reader, serialized with layer uploads.
func (s *LayerStore) prune(retained []string) error {
	keep := make(map[string]bool, len(retained))
	for _, value := range retained {
		digest, err := layerDigest(value)
		if err != nil {
			return err
		}
		keep[digest] = true
	}
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		name, marker := strings.CutSuffix(entry.Name(), ".done")
		digest, err := layerDigest(name)
		if err != nil || name != digest || keep[digest] {
			continue
		}
		if marker {
			if err := os.Remove(filepath.Join(s.dir, entry.Name())); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			continue
		}
		if err := os.Remove(filepath.Join(s.dir, digest+".done")); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err := os.RemoveAll(filepath.Join(s.dir, digest)); err != nil {
			return err
		}
	}
	return nil
}
