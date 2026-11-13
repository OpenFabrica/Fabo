//go:build linux || darwin

package localengine

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"math"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"

	"github.com/libracoder/kiri/engine/internal/hostagent"
)

const archiveEntryLimit = 100_000

type archiveFile struct {
	local, digest, link string
	size                int64
}

type stagedArchive struct {
	files map[string]archiveFile
}

func archiveName(raw string) (string, error) {
	for strings.HasPrefix(raw, "./") {
		raw = raw[2:]
	}
	if !fs.ValidPath(raw) || raw == "." {
		return "", fmt.Errorf("invalid archive path %q", raw)
	}
	return raw, nil
}

func readImageArchive(ctx context.Context, dir string, input io.Reader) (stagedArchive, error) {
	decoded, _, err := decodeArchive(input, true)
	if err != nil {
		return stagedArchive{}, err
	}
	defer decoded.Close()
	stream := archiveContextReader{ctx, decoded}
	reader := tar.NewReader(stream)
	staged := stagedArchive{files: make(map[string]archiveFile)}
	for count := 0; ; count++ {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return stagedArchive{}, err
		}
		if count == archiveEntryLimit {
			return stagedArchive{}, fmt.Errorf("too many archive entries")
		}
		if header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeLink {
			continue
		}
		key, err := archiveName(header.Name)
		if err != nil {
			return stagedArchive{}, err
		}
		if _, exists := staged.files[key]; exists {
			return stagedArchive{}, fmt.Errorf("duplicate archive entry %q", key)
		}
		if header.Typeflag == tar.TypeLink {
			link, err := archiveName(header.Linkname)
			if err != nil {
				return stagedArchive{}, err
			}
			staged.files[key] = archiveFile{link: link}
			continue
		}
		file, err := os.CreateTemp(dir, "member-")
		if err != nil {
			return stagedArchive{}, err
		}
		hash := sha256.New()
		size, err := io.Copy(io.MultiWriter(file, hash), reader)
		if err := errors.Join(err, file.Close()); err != nil {
			return stagedArchive{}, err
		}
		staged.files[key] = archiveFile{local: file.Name(), digest: fmt.Sprintf("%x", hash.Sum(nil)), size: size}
	}
	// A tar end marker can precede the codec checksum.
	if _, err := io.Copy(io.Discard, stream); err != nil {
		return stagedArchive{}, err
	}
	return staged, nil
}

func (s stagedArchive) file(ref string) (archiveFile, error) {
	key, err := archiveName(ref)
	if err != nil {
		return archiveFile{}, err
	}
	requested := key
	for range len(s.files) {
		file, ok := s.files[key]
		if !ok {
			break
		}
		if file.link == "" {
			for _, member := range []string{requested, key} {
				if digest, ok := strings.CutPrefix(member, "blobs/sha256/"); ok && (!validBlobName(digest) || file.digest != digest) {
					return archiveFile{}, fmt.Errorf("digest mismatch for %q", member)
				}
			}
			return file, nil
		}
		key = file.link
	}
	return archiveFile{}, fmt.Errorf("missing or circular archive entry %q", ref)
}

func (s stagedArchive) readJSON(ref string) ([]byte, error) {
	file, err := s.file(ref)
	if err != nil {
		return nil, err
	}
	if file.size > maxArchiveJSON {
		return nil, fmt.Errorf("archive metadata %q exceeds %d bytes", ref, maxArchiveJSON)
	}
	return os.ReadFile(file.local)
}

type imageImport struct {
	tags     map[string]Image
	untagged map[string]Image
	layers   map[string]archiveFile
}

func planImageImport(ctx context.Context, staged stagedArchive, platforms ...v1.Platform) (imageImport, error) {
	var entries []archiveManifestEntry
	raw, err := staged.readJSON("manifest.json")
	if err != nil {
		return imageImport{}, err
	}
	if err := json.Unmarshal(raw, &entries); err != nil || len(entries) == 0 {
		return imageImport{}, fmt.Errorf("invalid or empty Docker image manifest")
	}
	plan := imageImport{tags: make(map[string]Image), untagged: make(map[string]Image), layers: make(map[string]archiveFile)}
	type validatedLayer struct {
		diff, media string
	}
	validated := make(map[string]validatedLayer)
	for _, entry := range entries {
		config, err := staged.readJSON(entry.Config)
		if err != nil {
			return imageImport{}, err
		}
		image, _, err := imageFromConfig(config)
		if err != nil {
			return imageImport{}, err
		}
		if platform := image.platform(); !storableImagePlatform(platform) || !selectedPlatform(platform, platforms) {
			continue
		}
		if expected := strings.TrimSuffix(path.Base(entry.Config), ".json"); validBlobName(expected) && image.ID != "sha256:"+expected {
			return imageImport{}, fmt.Errorf("image config digest mismatch for %q", entry.Config)
		}
		if len(entry.Layers) != len(image.DiffIDs) {
			return imageImport{}, fmt.Errorf("image %s has a mismatched layer count", image.ID)
		}
		for i, ref := range entry.Layers {
			file, err := staged.file(ref)
			if err != nil {
				return imageImport{}, err
			}
			layer, ok := validated[file.digest]
			if !ok {
				layer.diff, layer.media, err = validateStagedLayer(ctx, file)
				if err != nil {
					return imageImport{}, fmt.Errorf("verify layer %q: %w", ref, err)
				}
				validated[file.digest] = layer
			}
			if layer.diff != image.DiffIDs[i] {
				return imageImport{}, fmt.Errorf("layer %q does not match image DiffID", ref)
			}
			if file.size > math.MaxInt64-image.Size {
				return imageImport{}, fmt.Errorf("image size overflow")
			}
			image.Size += file.size
			image.Layers = append(image.Layers, hostagent.Layer{Digest: "sha256:" + file.digest, MediaType: layer.media})
			plan.layers[file.digest] = file
		}
		if len(entry.RepoTags) == 0 {
			plan.untagged[image.ID] = image
		}
		for _, rawTag := range entry.RepoTags {
			tag, err := name.NewTag(rawTag)
			if err != nil {
				return imageImport{}, fmt.Errorf("invalid archive image reference %q", rawTag)
			}
			if prior, exists := plan.tags[tag.Name()]; exists && prior.ID != image.ID {
				if prior.platform().Equals(image.platform()) {
					return imageImport{}, fmt.Errorf("conflicting images for reference %q", rawTag)
				}
				// Like the Rust store, a tag points at one image. Keep the
				// previous architecture addressable by its config digest.
				plan.untagged[prior.ID] = prior
			}
			plan.tags[tag.Name()] = image
		}
	}
	if len(plan.tags)+len(plan.untagged) == 0 {
		return imageImport{}, fmt.Errorf("archive contains no images matching the requested platforms")
	}
	return plan, nil
}

func validateStagedLayer(ctx context.Context, file archiveFile) (string, string, error) {
	r, err := os.Open(file.local)
	if err != nil {
		return "", "", err
	}
	defer r.Close()
	return archiveLayerDiffID(ctx, r, file.size)
}

func (e *Engine) importImages(ctx context.Context, plan imageImport, action string) (resultErr error) {
	e.pulls.Lock()
	defer e.pulls.Unlock()
	blobs := filepath.Join(e.state.dir, "blobs")
	if err := os.MkdirAll(blobs, 0700); err != nil {
		return err
	}
	var installed []string
	defer func() {
		if resultErr != nil {
			for _, file := range installed {
				if err := os.Remove(file); err != nil && !errors.Is(err, os.ErrNotExist) {
					resultErr = errors.Join(resultErr, err)
				}
			}
		}
	}()
	for digest, layer := range plan.layers {
		if err := ctx.Err(); err != nil {
			return err
		}
		target := filepath.Join(blobs, digest)
		created, err := publishArchiveLayer(ctx, layer, target)
		if err != nil {
			return err
		}
		if created {
			installed = append(installed, target)
		}
	}
	addPaths := func(image Image) Image {
		image.Layers = slices.Clone(image.Layers)
		for i := range image.Layers {
			digest := strings.TrimPrefix(image.Layers[i].Digest, "sha256:")
			if _, staged := plan.layers[digest]; staged {
				image.Layers[i].Path = filepath.Join(blobs, digest)
			}
		}
		return image
	}
	for ref, image := range plan.tags {
		plan.tags[ref] = addPaths(image)
	}
	for id, image := range plan.untagged {
		plan.untagged[id] = addPaths(image)
	}

	e.imageOps.Lock()
	defer e.imageOps.Unlock()
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if e.closed {
		return fmt.Errorf("%w: daemon is shutting down", errConflict)
	}
	if err := e.putImages(plan.tags, plan.untagged); err != nil {
		return err
	}
	for _, ref := range slices.Sorted(maps.Keys(plan.tags)) {
		e.emitImage(plan.tags[ref], action, ref)
	}
	for _, id := range slices.Sorted(maps.Keys(plan.untagged)) {
		e.emitImage(plan.untagged[id], action, id)
	}
	return nil
}

func publishArchiveLayer(ctx context.Context, layer archiveFile, target string) (bool, error) {
	source, err := os.OpenFile(layer.local, os.O_RDWR, 0)
	if err != nil {
		return false, err
	}
	if err := errors.Join(source.Sync(), source.Close()); err != nil {
		return false, err
	}
	err = os.Link(layer.local, target)
	if err == nil {
		return true, nil
	}
	if !errors.Is(err, os.ErrExist) {
		return false, err
	}
	existing, err := os.Open(target)
	if err != nil {
		return false, err
	}
	defer existing.Close()
	hash := sha256.New()
	n, err := io.Copy(hash, archiveContextReader{ctx, existing})
	if err != nil {
		return false, err
	}
	if n != layer.size || fmt.Sprintf("%x", hash.Sum(nil)) != layer.digest {
		return false, fmt.Errorf("cached layer %s does not match the archive", layer.digest)
	}
	return false, nil
}

type archiveUpload struct {
	r          io.Reader
	controller *http.ResponseController
}

func (u archiveUpload) Read(p []byte) (int, error) {
	// The server's short request deadline is appropriate for JSON APIs.
	// Give large uploads an idle deadline instead of a total deadline.
	_ = u.controller.SetReadDeadline(time.Now().Add(30 * time.Second))
	return u.r.Read(p)
}

func (e *Engine) loadImageHTTP(w http.ResponseWriter, r *http.Request) {
	platforms, err := imagePlatforms(r.URL.Query()["platform"])
	if err != nil {
		writeError(w, err)
		return
	}
	dir, err := os.MkdirTemp(e.state.dir, ".image-archive-")
	if err != nil {
		writeError(w, err)
		return
	}
	defer os.RemoveAll(dir)
	staged, err := readImageArchive(r.Context(), dir, archiveUpload{r.Body, http.NewResponseController(w)})
	if err != nil {
		writeError(w, fmt.Errorf("%w: read image archive: %v", errInvalid, err))
		return
	}
	plan, err := planImageImport(r.Context(), staged, platforms...)
	if err != nil {
		writeError(w, fmt.Errorf("%w: %v", errInvalid, err))
		return
	}
	if err := e.importImages(r.Context(), plan, "load"); err != nil {
		writeError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	out := json.NewEncoder(w)
	for _, tag := range slices.Sorted(maps.Keys(plan.tags)) {
		if err := out.Encode(map[string]string{"stream": "Loaded image: " + shortImageReference(tag) + "\n"}); err != nil {
			return
		}
	}
	for _, id := range slices.Sorted(maps.Keys(plan.untagged)) {
		if err := out.Encode(map[string]string{"stream": "Loaded image ID: " + id + "\n"}); err != nil {
			return
		}
	}
}
