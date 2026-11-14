//go:build linux || darwin

package localengine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	"github.com/OpenFabrica/FabriBay/engine/internal/hostagent"
)

func (e *Engine) pull(ctx context.Context, ref name.Reference, auth authn.AuthConfig, platforms ...v1.Platform) (Image, error) {
	wanted := nativePlatform()
	if len(platforms) != 0 {
		wanted = platforms[0]
	}
	allowFallback := len(platforms) == 0 && e.rosetta && wanted.Architecture == "arm64"
	descriptor, err := remote.Get(ref, remote.WithContext(ctx), remote.WithPlatform(wanted),
		remote.WithTransport(e.registryTransport()),
		remote.WithAuth(authn.FromConfig(auth)))
	if err != nil {
		return Image{}, err
	}
	remoteImage, selected, err := selectRegistryImage(descriptor, wanted, allowFallback)
	if err != nil {
		return Image{}, err
	}
	// Reclamation takes the exclusive side of this lock. Container
	// creation and tagging can continue during registry downloads.
	e.pulls.RLock()
	defer e.pulls.RUnlock()
	image, err := cacheImage(ctx, filepath.Join(e.state.dir, "blobs"), remoteImage, selected, allowFallback && !descriptor.MediaType.IsIndex())
	if err != nil {
		return Image{}, err
	}

	if digest, err := remoteImage.Digest(); err == nil {
		image.RepoDigest = ref.Context().Name() + "@" + digest.String()
	}
	e.imageOps.Lock()
	defer e.imageOps.Unlock()
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return Image{}, fmt.Errorf("daemon is shutting down")
	}
	if err := e.putImage(ref.Name(), image); err != nil {
		return Image{}, err
	}
	e.emitImage(image, "pull", ref.Name())
	return image, nil
}

// Resolve only the requested manifest before fetching any image layers. An
// implicit pull prefers native even when the registry lists amd64 first.
func selectRegistryImage(desc *remote.Descriptor, wanted v1.Platform, allowFallback bool) (v1.Image, v1.Platform, error) {
	if desc.MediaType.IsIndex() {
		index, err := desc.ImageIndex()
		if err != nil {
			return nil, wanted, err
		}
		manifest, err := index.IndexManifest()
		if err != nil {
			return nil, wanted, err
		}
		var fallback *v1.Descriptor
		amd64 := v1.Platform{OS: "linux", Architecture: "amd64"}
		for _, child := range manifest.Manifests {
			if child.Platform == nil {
				continue
			}
			if matchesImagePlatform(*child.Platform, wanted) {
				if child.MediaType.IsIndex() {
					image, err := desc.Image()
					return image, wanted, err
				}
				image, err := index.Image(child.Digest)
				return image, wanted, err
			}
			if allowFallback && fallback == nil && !child.MediaType.IsIndex() && matchesImagePlatform(*child.Platform, amd64) {
				fallback = &child
			}
		}
		if fallback != nil {
			image, err := index.Image(fallback.Digest)
			return image, amd64, err
		}
	}
	image, err := desc.Image()
	return image, wanted, err
}

func cacheImage(ctx context.Context, dir string, source v1.Image, platform v1.Platform, allowFallback bool) (Image, error) {
	manifest, err := source.Manifest()
	if err != nil {
		return Image{}, err
	}
	raw, err := source.RawConfigFile()
	if err != nil {
		return Image{}, err
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return Image{}, err
	}
	image, _, err := imageFromConfig(raw)
	if err != nil {
		return Image{}, err
	}
	actual := image.platform()
	if !matchesImagePlatform(actual, platform) && !(allowFallback && actual.OS == "linux" && actual.Architecture == "amd64") {
		return Image{}, fmt.Errorf("image platform %s does not match requested %s", actual, platform)
	}
	if image.ID != manifest.Config.Digest.String() {
		return Image{}, fmt.Errorf("image config digest mismatch")
	}
	if len(image.DiffIDs) != len(manifest.Layers) {
		return Image{}, fmt.Errorf("image config and manifest have different layer counts")
	}
	for _, layer := range manifest.Layers {
		if err := ctx.Err(); err != nil {
			return Image{}, err
		}
		if layer.Size > math.MaxInt64-image.Size {
			return Image{}, fmt.Errorf("image size overflow")
		}
		path, err := cacheBlob(dir, source, layer)
		if err != nil {
			return Image{}, fmt.Errorf("cache layer %s: %w", layer.Digest, err)
		}
		image.Layers = append(image.Layers, hostagent.Layer{
			Digest: layer.Digest.String(), MediaType: string(layer.MediaType), Path: path,
		})
		image.Size += layer.Size
	}
	return image, nil
}

func cacheBlob(dir string, image v1.Image, descriptor v1.Descriptor) (string, error) {
	digest, err := hex.DecodeString(descriptor.Digest.Hex)
	if err != nil || descriptor.Digest.Algorithm != "sha256" || len(digest) != sha256.Size ||
		descriptor.Size < 0 || descriptor.Size == math.MaxInt64 {
		return "", fmt.Errorf("invalid layer descriptor")
	}
	path := filepath.Join(dir, descriptor.Digest.Hex)
	if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() && info.Size() == descriptor.Size {
		return path, nil
	}
	layer, err := image.LayerByDigest(descriptor.Digest)
	if err != nil {
		return "", err
	}
	stream, err := layer.Compressed()
	if err != nil {
		return "", err
	}
	defer stream.Close()
	tmp, err := os.CreateTemp(dir, ".blob-")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	hash := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, hash), io.LimitReader(stream, descriptor.Size+1))
	if err != nil {
		return "", err
	}
	if n != descriptor.Size || hex.EncodeToString(hash.Sum(nil)) != descriptor.Digest.Hex {
		return "", fmt.Errorf("layer size or digest mismatch")
	}
	if err := tmp.Sync(); err != nil {
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	return path, os.Rename(tmp.Name(), path)
}
