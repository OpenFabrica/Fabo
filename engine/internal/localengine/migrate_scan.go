//go:build linux || darwin

package localengine

import (
	"context"
	"errors"
	"os"
	"path"
	"path/filepath"
	"strconv"
)

func migrateKiriScans(ctx context.Context, source *os.Root, stage string, index imageIndex) error {
	engine := &Engine{images: index.references(nil), imageIndex: index}
	for id := range engine.allImages() {
		if err := ctx.Err(); err != nil {
			return err
		}
		digest, err := kiriDigest(id)
		if err != nil {
			return err
		}
		dir := path.Join("scans", digest)
		from, err := source.Open(dir)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		entries, err := from.ReadDir(-1)
		from.Close()
		if err != nil {
			return err
		}
		to := filepath.Join(stage, "scans", digest)
		for _, entry := range entries {
			at, err := strconv.ParseInt(entry.Name(), 10, 64)
			if err == nil && at >= 0 && entry.Name() == strconv.FormatInt(at, 10) && entry.IsDir() {
				if err := copyKiriScan(ctx, source, path.Join(dir, entry.Name()), filepath.Join(to, entry.Name())); err != nil {
					return err
				}
			}
		}
		// Stores that predate scan history put one report at the image root.
		if info, err := source.Lstat(path.Join(dir, "grype.json")); err == nil {
			at := info.ModTime().Unix()
			dest := filepath.Join(to, strconv.FormatInt(at, 10))
			for _, err := os.Lstat(dest); err == nil; _, err = os.Lstat(dest) {
				at++
				dest = filepath.Join(to, strconv.FormatInt(at, 10))
			}
			if err := copyKiriScan(ctx, source, dir, dest); err != nil {
				return err
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

func copyKiriScan(ctx context.Context, source *os.Root, relative, destination string) error {
	if _, err := source.Lstat(path.Join(relative, "grype.json")); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	if err := os.MkdirAll(destination, 0700); err != nil {
		return err
	}
	for _, file := range []string{"grype.json", "sbom.cdx.json", "summary.json"} {
		if err := copyKiriFile(ctx, source, path.Join(relative, file), filepath.Join(destination, file)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}
