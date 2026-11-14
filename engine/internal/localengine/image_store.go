//go:build linux || darwin

package localengine

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/google/go-containerregistry/pkg/name"

	"github.com/OpenFabrica/Fabo/engine/internal/hostagent"
)

// A nil override hides a reference supplied by the startup catalog.
type imageIndex struct {
	Overrides map[string]*Image
	Dangling  map[string]Image
}

func (i imageIndex) clone() imageIndex {
	next := imageIndex{Overrides: maps.Clone(i.Overrides), Dangling: maps.Clone(i.Dangling)}
	if next.Overrides == nil {
		next.Overrides = make(map[string]*Image)
	}
	if next.Dangling == nil {
		next.Dangling = make(map[string]Image)
	}
	return next
}

func (i imageIndex) references(catalog map[string]Image) map[string]Image {
	refs := make(map[string]Image, len(catalog)+len(i.Overrides))
	maps.Copy(refs, catalog)
	for ref, image := range i.Overrides {
		if image == nil {
			delete(refs, ref)
		} else {
			refs[ref] = *image
		}
	}
	return refs
}

// Catalog fixtures can omit a config digest. Give their metadata and layer
// digests a stable local identity, independent of the archive file paths.
func identifyImage(image Image) Image {
	if image.ID != "" {
		return image
	}
	identity := image
	identity.Layers = slices.Clone(identity.Layers)
	for i := range identity.Layers {
		identity.Layers[i].Path = ""
	}
	raw, _ := json.Marshal(identity)
	image.ID = fmt.Sprintf("sha256:%x", sha256.Sum256(raw))
	return image
}

// commitImages updates the in-memory view only after the durable index write.
// The caller holds e.mu and imageOps.
func (e *Engine) commitImages(next imageIndex) error {
	if e.state != nil {
		if err := saveJSON(e.state.dir, "image-index.json", next); err != nil {
			return fmt.Errorf("save image index: %w", err)
		}
	}
	e.imageIndex = next
	e.images = next.references(e.catalog)
	return nil
}

func (e *Engine) removeImageRef(index *imageIndex, ref string) {
	if _, seeded := e.catalog[ref]; seeded {
		index.Overrides[ref] = nil
	} else {
		delete(index.Overrides, ref)
	}
}

func (e *Engine) putImage(ref string, image Image) error {
	return e.putImages(map[string]Image{ref: image}, nil)
}

func (e *Engine) putImages(tags, untagged map[string]Image) error {
	next := e.imageIndex.clone()
	maps.Copy(next.Dangling, untagged)
	replaced := make(map[string]Image)
	for ref, image := range tags {
		if key, prior, ok := e.imageReference(ref); ok {
			ref = key
			if prior.ID != image.ID {
				replaced[prior.ID] = prior
			} else if image.RepoDigest == "" {
				image.RepoDigest = prior.RepoDigest
			}
		}
		value := image
		next.Overrides[ref] = &value
	}
	maps.Copy(next.Dangling, replaced)
	for _, image := range next.references(e.catalog) {
		delete(next.Dangling, identifyImage(image).ID)
	}
	return e.commitImages(next)
}

// imageReference resolves a tagged or digest-pinned name, never an image ID.
func (e *Engine) imageReference(key string) (string, Image, bool) {
	if image, ok := e.images[key]; ok {
		return key, identifyImage(image), true
	}
	ref, err := name.ParseReference(key)
	if err != nil {
		return "", Image{}, false
	}
	canonical := ref.Name()
	if image, ok := e.images[canonical]; ok {
		return canonical, identifyImage(image), true
	}
	for ref, image := range e.images {
		if parsed, err := name.ParseReference(ref); err == nil && parsed.Name() == canonical {
			return ref, identifyImage(image), true
		}
	}
	return "", Image{}, false
}

type selectedImage struct {
	ref string
	Image
}

// findImage resolves a reference, config digest or an unambiguous digest
// prefix. The caller holds e.mu.
func (e *Engine) findImage(key string) (selectedImage, error) {
	if ref, image, ok := e.imageReference(key); ok {
		return selectedImage{ref, image}, nil
	}
	if strings.Contains(key, "@") {
		if digest, err := name.NewDigest(key); err == nil {
			for _, image := range e.images {
				if image.RepoDigest == digest.Name() {
					return selectedImage{Image: identifyImage(image)}, nil
				}
			}
		}
	}
	prefix := strings.TrimPrefix(key, "sha256:")
	valid := len(prefix) >= 4 && len(prefix) <= 64
	if valid {
		_, err := hex.DecodeString(prefix + strings.Repeat("0", len(prefix)%2))
		valid = err == nil
	}
	var found Image
	if valid {
		for _, image := range e.allImages() {
			id := strings.TrimPrefix(image.ID, "sha256:")
			if !strings.HasPrefix(id, strings.ToLower(prefix)) {
				continue
			}
			if found.ID != "" && found.ID != image.ID {
				return selectedImage{}, fmt.Errorf("%w: ambiguous image ID %q", errConflict, key)
			}
			found = image
		}
	}
	if found.ID == "" {
		return selectedImage{}, fmt.Errorf("%w: image %q", errMissing, key)
	}
	return selectedImage{Image: found}, nil
}

func (e *Engine) allImages() map[string]Image {
	all := make(map[string]Image, len(e.images)+len(e.imageIndex.Dangling))
	maps.Copy(all, e.imageIndex.Dangling)
	refs := slices.Sorted(maps.Keys(e.images))
	for _, ref := range slices.Backward(refs) {
		image := identifyImage(e.images[ref])
		all[image.ID] = image
	}
	return all
}

func (e *Engine) imageRefs(id string) []string {
	var refs []string
	for ref, image := range e.images {
		if identifyImage(image).ID == id {
			refs = append(refs, ref)
		}
	}
	slices.Sort(refs)
	return refs
}

// Older records kept the layer list but no image ID. Protect those layers
// and conservatively treat a matching image as referenced.
func imageUsedBy(image Image, c *container) bool {
	if c.imageID != "" {
		return c.imageID == image.ID
	}
	return slices.EqualFunc(c.blobs, image.Layers, func(a, b hostagent.Layer) bool { return a.Digest == b.Digest })
}
