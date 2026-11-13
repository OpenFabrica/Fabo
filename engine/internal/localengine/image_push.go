//go:build linux || darwin

package localengine

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path"
	"slices"
	"strings"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/partial"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

// A registry image keeps the config bytes and the cached layer bytes intact.
// The OCI manifest is derived from the snapshot's actual blob descriptors.
type pushImage struct {
	config, manifest []byte
	layers           map[v1.Hash]pushLayer
}

type pushLayer struct {
	v1.Descriptor
	exportLayer
}

func (l pushLayer) Digest() (v1.Hash, error) { return l.Descriptor.Digest, nil }
func (l pushLayer) Size() (int64, error)     { return l.Descriptor.Size, nil }
func (l pushLayer) MediaType() (types.MediaType, error) {
	return l.Descriptor.MediaType, nil
}
func (l pushLayer) Compressed() (io.ReadCloser, error) {
	return io.NopCloser(io.NewSectionReader(l.file, 0, l.size)), nil
}

func (p pushImage) RawConfigFile() ([]byte, error) { return p.config, nil }
func (p pushImage) RawManifest() ([]byte, error)   { return p.manifest, nil }
func (p pushImage) MediaType() (types.MediaType, error) {
	return types.OCIManifestSchema1, nil
}
func (p pushImage) LayerByDigest(digest v1.Hash) (partial.CompressedLayer, error) {
	layer, ok := p.layers[digest]
	if !ok {
		return nil, fmt.Errorf("missing push layer %s", digest)
	}
	return layer, nil
}

func (x imageExport) registryImage(ctx context.Context, image Image) (pushImage, error) {
	raw, err := x.config(ctx, image)
	if err != nil {
		return pushImage{}, err
	}
	configHash, configSize, err := v1.SHA256(bytes.NewReader(raw))
	if err != nil {
		return pushImage{}, err
	}
	manifest := v1.Manifest{
		SchemaVersion: 2,
		MediaType:     types.OCIManifestSchema1,
		Config:        v1.Descriptor{Digest: configHash, Size: configSize, MediaType: types.OCIConfigJSON},
		Layers:        make([]v1.Descriptor, 0, len(image.Layers)),
	}
	p := pushImage{config: raw, layers: make(map[v1.Hash]pushLayer)}
	for _, layer := range image.Layers {
		digest, err := v1.NewHash(layer.Digest)
		if err != nil {
			return pushImage{}, err
		}
		media := types.MediaType(layer.MediaType)
		switch media {
		case types.DockerLayer, types.OCILayer:
			media = types.OCILayer
		case types.DockerUncompressedLayer, types.OCIUncompressedLayer:
			media = types.OCIUncompressedLayer
		case types.OCILayerZStd:
		default:
			return pushImage{}, fmt.Errorf("cannot push layer %s with media type %q", digest, media)
		}
		f := x.layer(layer)
		descriptor := v1.Descriptor{Digest: digest, MediaType: media, Size: f.size}
		manifest.Layers = append(manifest.Layers, descriptor)
		p.layers[digest] = pushLayer{Descriptor: descriptor, exportLayer: f}
	}
	p.manifest, err = json.Marshal(manifest)
	return p, err
}

func (e *Engine) pushTargets(ref, tag string) ([]name.Tag, error) {
	if ref == "" || strings.Contains(ref, "@") {
		return nil, fmt.Errorf("%w: expected an image repository or tag", errInvalid)
	}
	if tag != "" || strings.Contains(path.Base(ref), ":") {
		if tag != "" {
			ref += ":" + tag
		}
		target, err := name.NewTag(ref)
		if err != nil {
			return nil, fmt.Errorf("%w: invalid push tag", errInvalid)
		}
		return []name.Tag{target}, nil
	}
	repo, err := name.NewRepository(ref)
	if err != nil {
		return nil, fmt.Errorf("%w: invalid push repository", errInvalid)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	var targets []name.Tag
	seen := make(map[string]bool)
	for ref := range e.images {
		if candidate, err := name.NewTag(ref); err == nil && candidate.Context().Name() == repo.Name() && !seen[candidate.Name()] {
			targets = append(targets, candidate)
			seen[candidate.Name()] = true
		}
	}
	if len(targets) == 0 {
		return nil, fmt.Errorf("%w: no local tags for %q", errMissing, repo.Name())
	}
	slices.SortFunc(targets, func(a, b name.Tag) int { return strings.Compare(a.TagStr(), b.TagStr()) })
	return targets, nil
}

func (e *Engine) pushImageHTTP(w http.ResponseWriter, r *http.Request, ref string) {
	targets, err := e.pushTargets(ref, r.URL.Query().Get("tag"))
	if err != nil {
		writeError(w, err)
		return
	}
	auth, err := registryAuth(r)
	if err != nil {
		writeError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	out := json.NewEncoder(w)
	report := func(event any) error {
		if err := out.Encode(event); err != nil {
			return err
		}
		return http.NewResponseController(w).Flush()
	}
	if err := report(map[string]string{"status": "The push refers to repository [" + targets[0].Context().Name() + "]"}); err != nil {
		return
	}
	for _, target := range targets {
		if err := e.pushTag(r.Context(), target, auth, report); err != nil {
			writeRegistryStreamError(out, err, "push denied")
			return
		}
	}
}

func (e *Engine) pushTag(ctx context.Context, target name.Tag, auth authn.AuthConfig, report func(any) error) error {
	snapshot, err := e.openImageExport([]string{target.Name()})
	if err != nil {
		return err
	}
	defer snapshot.close()
	image := snapshot.images[0].Image
	payload, err := snapshot.registryImage(ctx, image)
	if err != nil {
		return err
	}
	source, err := partial.CompressedToImage(payload)
	if err != nil {
		return err
	}
	digest, err := source.Digest()
	if err != nil {
		return err
	}
	options := []remote.Option{remote.WithContext(ctx), remote.WithAuth(authn.FromConfig(auth)), remote.WithTransport(e.registryTransport())}
	// The registry verifies content digests and skips blobs it already has.
	// An identical manifest at the tag also tells the CLI this is a repeat push.
	status := "Pushing " + target.TagStr()
	if existing, err := remote.Head(target, options...); err == nil && existing.Digest == digest {
		status = "Layer already exists"
	}
	if err := report(map[string]string{"status": status}); err != nil {
		return err
	}
	if err := remote.Write(target, source, options...); err != nil {
		return err
	}
	e.emitImage(image, "push", target.Name())
	if err := e.recordPushedDigest(target, image.ID, digest.String()); err != nil {
		return fmt.Errorf("image uploaded, but recording its digest failed: %w", err)
	}
	size := len(payload.manifest)
	if err := report(map[string]string{"status": fmt.Sprintf("%s: digest: %s size: %d", target.TagStr(), digest, size)}); err != nil {
		return err
	}
	return report(map[string]any{"aux": map[string]any{"Tag": target.TagStr(), "Digest": digest.String(), "Size": size}})
}

func (e *Engine) recordPushedDigest(target name.Tag, imageID, digest string) error {
	e.imageOps.Lock()
	defer e.imageOps.Unlock()
	e.mu.Lock()
	defer e.mu.Unlock()
	key, current, ok := e.imageReference(target.Name())
	// The upload used a snapshot. Don't recreate or overwrite a tag that
	// was removed or retargeted while the registry transfer was in flight.
	if e.closed || !ok || current.ID != imageID {
		return nil
	}
	current.RepoDigest = target.Context().Name() + "@" + digest
	return e.putImage(key, current)
}
