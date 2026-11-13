//go:build linux || darwin

package localengine

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

func (e *Engine) writeScanLayout(ctx context.Context, image Image, dir string) error {
	e.imageOps.Lock()
	export, err := openImageLayers([]exportImage{{Image: image}})
	e.imageOps.Unlock()
	if err != nil {
		return err
	}
	defer export.close()
	blobs := filepath.Join(dir, "blobs", "sha256")
	if err := os.MkdirAll(blobs, 0700); err != nil {
		return err
	}
	put := func(media types.MediaType, raw []byte) (v1.Descriptor, error) {
		hex := fmt.Sprintf("%x", sha256.Sum256(raw))
		desc := v1.Descriptor{MediaType: media, Digest: v1.Hash{Algorithm: "sha256", Hex: hex}, Size: int64(len(raw))}
		return desc, os.WriteFile(filepath.Join(blobs, hex), raw, 0600)
	}
	config, err := export.config(ctx, image)
	if err != nil {
		return err
	}
	manifest := v1.Manifest{SchemaVersion: 2, MediaType: types.OCIManifestSchema1, Layers: make([]v1.Descriptor, 0, len(image.Layers))}
	manifest.Config, err = put(types.OCIConfigJSON, config)
	if err != nil {
		return err
	}
	linked := make(map[string]bool, len(image.Layers))
	for _, layer := range image.Layers {
		if err := ctx.Err(); err != nil {
			return err
		}
		digest, err := v1.NewHash(layer.Digest)
		if err != nil || digest.Algorithm != "sha256" || !validBlobName(digest.Hex) {
			return fmt.Errorf("invalid image layer digest %q", layer.Digest)
		}
		source := export.layer(layer)
		if !linked[digest.Hex] {
			path := filepath.Join(blobs, digest.Hex)
			if err := os.Link(source.file.Name(), path); err != nil {
				if err := copyScanLayer(ctx, path, source); err != nil {
					return err
				}
			}
			linked[digest.Hex] = true
		}
		manifest.Layers = append(manifest.Layers, v1.Descriptor{
			MediaType: types.MediaType(layer.MediaType), Digest: digest, Size: source.size,
		})
	}
	raw, err := json.Marshal(manifest)
	if err != nil {
		return err
	}
	desc, err := put(types.OCIManifestSchema1, raw)
	if err != nil {
		return err
	}
	raw, err = json.Marshal(v1.IndexManifest{SchemaVersion: 2, MediaType: types.OCIImageIndex, Manifests: []v1.Descriptor{desc}})
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "index.json"), raw, 0600); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "oci-layout"), []byte(`{"imageLayoutVersion":"1.0.0"}`), 0600)
}

func copyScanLayer(ctx context.Context, path string, source exportLayer) error {
	out, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, archiveContextReader{ctx, io.NewSectionReader(source.file, 0, source.size)})
	return errors.Join(err, out.Close())
}
