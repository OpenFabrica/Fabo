//go:build linux || darwin

package localengine

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"

	"github.com/libracoder/kiri/engine/internal/hostagent"
)

type exportImage struct {
	Image
	tags []string
}

func (e *Engine) selectImageExports(names []string) ([]exportImage, error) {
	var result []exportImage
	byID := make(map[string]int)
	refs := slices.Sorted(maps.Keys(e.images))
	add := func(image selectedImage) {
		i, exists := byID[image.ID]
		if !exists {
			i = len(result)
			byID[image.ID] = i
			result = append(result, exportImage{Image: image.Image})
		}
		if image.ref != "" {
			// A digest-only reference stays untagged. A save file
			// does not carry a registry manifest for the digest.
			if tag, err := name.NewTag(image.ref); err == nil {
				ref := shortImageReference(tag.Name())
				if !slices.Contains(result[i].tags, ref) {
					result[i].tags = append(result[i].tags, ref)
				}
			}
		}
	}
	for _, selector := range names {
		if selector == "" {
			return nil, fmt.Errorf("%w: empty image export name", errInvalid)
		}
		var matchedRepo bool
		if repo, err := name.NewRepository(selector); err == nil {
			for _, ref := range refs {
				if parsed, err := name.ParseReference(ref); err == nil && parsed.Context().Name() == repo.Name() {
					add(selectedImage{ref, identifyImage(e.images[ref])})
					matchedRepo = true
				}
			}
		}
		if matchedRepo {
			continue
		}
		image, err := e.findImage(selector)
		if err != nil {
			return nil, err
		}
		add(image)
	}
	slices.SortFunc(result, func(a, b exportImage) int { return strings.Compare(a.ID, b.ID) })
	for i := range result {
		slices.Sort(result[i].tags)
	}
	return result, nil
}

type exportLayer struct {
	file *os.File
	size int64
}

type imageExport struct {
	images []exportImage
	layers map[string]exportLayer
}

func (e *Engine) openImageExport(names []string, platforms ...v1.Platform) (imageExport, error) {
	e.imageOps.Lock()
	defer e.imageOps.Unlock()
	e.mu.Lock()
	images, err := e.selectImageExports(names)
	e.mu.Unlock()
	if err != nil {
		return imageExport{}, err
	}
	images = slices.DeleteFunc(images, func(i exportImage) bool {
		return !selectedPlatform(i.platform(), platforms)
	})
	if len(images) == 0 {
		return imageExport{}, fmt.Errorf("%w: no images matching the requested platforms", errMissing)
	}
	return openImageLayers(images)
}

func openImageLayers(images []exportImage) (imageExport, error) {
	export := imageExport{images: images, layers: make(map[string]exportLayer)}
	for _, image := range images {
		for _, layer := range image.Layers {
			digest := strings.ToLower(strings.TrimPrefix(layer.Digest, "sha256:"))
			if !validBlobName(digest) {
				export.close()
				return imageExport{}, fmt.Errorf("invalid layer digest %q", layer.Digest)
			}
			if _, ok := export.layers[digest]; ok {
				continue
			}
			file, err := os.Open(layer.Path)
			if err != nil {
				export.close()
				return imageExport{}, fmt.Errorf("open image layer %s: %w", digest, err)
			}
			info, err := file.Stat()
			if err != nil || !info.Mode().IsRegular() {
				file.Close()
				export.close()
				return imageExport{}, fmt.Errorf("image layer %s is not a regular file", digest)
			}
			export.layers[digest] = exportLayer{file, info.Size()}
		}
	}
	// The open files pin the layer data even if another request prunes
	// their paths while the client is reading this archive.
	return export, nil
}

func (x imageExport) close() {
	for _, layer := range x.layers {
		_ = layer.file.Close()
	}
}

func (x imageExport) config(ctx context.Context, image Image) ([]byte, error) {
	if len(image.ConfigBlob) != 0 {
		if digest := fmt.Sprintf("sha256:%x", sha256.Sum256(image.ConfigBlob)); digest != image.ID {
			return nil, fmt.Errorf("stored config does not match image %s", image.ID)
		}
		return image.ConfigBlob, nil
	}

	// Older records stored only the fields needed to run the container.
	// Reconstruct an exportable config and derive its ID from its bytes.
	platform := image.platform()
	config := v1.ConfigFile{
		Architecture: platform.Architecture, OS: platform.OS, Variant: platform.Variant, Created: v1.Time{Time: time.Unix(image.Created, 0).UTC()},
		Config: v1.Config{
			Env: image.Env, Entrypoint: image.Entrypoint, Cmd: image.Cmd, WorkingDir: image.WorkingDir,
			User: image.User, StopSignal: image.StopSignal, Labels: image.Labels, ExposedPorts: image.ExposedPorts,
		},
		RootFS: v1.RootFS{Type: "layers", DiffIDs: make([]v1.Hash, 0, len(image.Layers))},
	}
	for i, layer := range image.Layers {
		var diff string
		if len(image.DiffIDs) == len(image.Layers) {
			diff = image.DiffIDs[i]
		} else {
			f := x.layer(layer)
			var err error
			diff, _, err = archiveLayerDiffID(ctx, io.NewSectionReader(f.file, 0, f.size), f.size)
			if err != nil {
				return nil, fmt.Errorf("read image %s layer %s: %w", image.ID, layer.Digest, err)
			}
		}
		hash, err := v1.NewHash(diff)
		if err != nil {
			return nil, err
		}
		config.RootFS.DiffIDs = append(config.RootFS.DiffIDs, hash)
	}
	return json.Marshal(config)
}

func (x imageExport) layer(layer hostagent.Layer) exportLayer {
	return x.layers[strings.ToLower(strings.TrimPrefix(layer.Digest, "sha256:"))]
}

type exportArchive struct {
	entries  []archiveManifestEntry
	configs  [][]byte
	manifest []byte
}

func (x imageExport) prepare(ctx context.Context) (exportArchive, error) {
	var archive exportArchive
	for _, image := range x.images {
		raw, err := x.config(ctx, image.Image)
		if err != nil {
			return exportArchive{}, err
		}
		if len(raw) > maxArchiveJSON {
			return exportArchive{}, fmt.Errorf("image config exceeds %d bytes", maxArchiveJSON)
		}
		entry := archiveManifestEntry{
			Config:   fmt.Sprintf("blobs/sha256/%x", sha256.Sum256(raw)),
			RepoTags: append([]string{}, image.tags...),
			Layers:   make([]string, 0, len(image.Layers)),
		}
		for _, layer := range image.Layers {
			entry.Layers = append(entry.Layers, "blobs/sha256/"+strings.ToLower(strings.TrimPrefix(layer.Digest, "sha256:")))
		}
		archive.entries = append(archive.entries, entry)
		archive.configs = append(archive.configs, raw)
	}
	manifest, err := json.Marshal(archive.entries)
	if err != nil {
		return exportArchive{}, err
	}
	if len(manifest) > maxArchiveJSON {
		return exportArchive{}, fmt.Errorf("image manifest exceeds %d bytes", maxArchiveJSON)
	}
	archive.manifest = manifest
	return archive, nil
}

func (x imageExport) write(ctx context.Context, w io.Writer, archive exportArchive) error {
	tw := tar.NewWriter(w)
	written := make(map[string]bool)
	write := func(path string, size int64, reader io.Reader) error {
		if err := tw.WriteHeader(&tar.Header{Name: path, Typeflag: tar.TypeReg, Mode: 0644, Size: size}); err != nil {
			return err
		}
		_, err := io.CopyN(tw, archiveContextReader{ctx, reader}, size)
		return err
	}
	for i, entry := range archive.entries {
		if !written[entry.Config] {
			raw := archive.configs[i]
			if err := write(entry.Config, int64(len(raw)), bytes.NewReader(raw)); err != nil {
				return err
			}
			written[entry.Config] = true
		}
		for _, path := range entry.Layers {
			if written[path] {
				continue
			}
			digest := strings.TrimPrefix(path, "blobs/sha256/")
			layer := x.layers[digest]
			hash := sha256.New()
			if err := write(path, layer.size, io.TeeReader(io.NewSectionReader(layer.file, 0, layer.size), hash)); err != nil {
				return err
			}
			if fmt.Sprintf("%x", hash.Sum(nil)) != digest {
				return fmt.Errorf("stored layer %s failed digest verification", digest)
			}
			written[path] = true
		}
	}
	if err := write("manifest.json", int64(len(archive.manifest)), bytes.NewReader(archive.manifest)); err != nil {
		return err
	}
	return tw.Close()
}

func (e *Engine) saveImagesHTTP(w http.ResponseWriter, r *http.Request) {
	platforms, err := imagePlatforms(r.URL.Query()["platform"])
	if err != nil {
		writeError(w, err)
		return
	}
	names := r.URL.Query()["names"]
	if len(names) == 0 {
		writeError(w, fmt.Errorf("%w: no image names supplied", errInvalid))
		return
	}
	export, err := e.openImageExport(names, platforms...)
	if err != nil {
		writeError(w, err)
		return
	}
	defer export.close()
	archive, err := export.prepare(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/x-tar")
	w.WriteHeader(http.StatusOK)
	if err := export.write(r.Context(), w, archive); err != nil {
		if !errors.Is(err, context.Canceled) {
			// Terminate the response so the client detects a partial tar.
			panic(http.ErrAbortHandler)
		}
		return
	}
	for _, image := range export.images {
		if len(image.tags) == 0 {
			e.emitImage(image.Image, "save", image.ID)
		}
		for _, tag := range image.tags {
			e.emitImage(image.Image, "save", tag)
		}
	}
}
