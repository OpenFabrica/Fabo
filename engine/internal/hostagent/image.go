//go:build linux || darwin

package hostagent

import (
	"context"
	"fmt"
	"os"

	"github.com/OpenFabrica/FabriBay/engine/internal/guestwire"
)

// Layer identifies a local OCI blob by its digest and media type.
type Layer struct {
	Digest, MediaType, Path string
}

// StartImage ensures each layer in spec is present before starting the
// container. The spec retains the caller's top-to-base layer order.
func StartImage(ctx context.Context, session *guestwire.Session, spec guestwire.RunSpec, layers []Layer) (uint32, error) {
	if err := EnsureLayers(ctx, session, spec.Layers, layers); err != nil {
		return 0, err
	}
	return session.Start(ctx, spec)
}

// EnsureLayers makes image layers available to a guest operation.
func EnsureLayers(ctx context.Context, session *guestwire.Session, digests []string, layers []Layer) error {
	blobs := make(map[string]Layer, len(layers))
	for _, layer := range layers {
		blobs[layer.Digest] = layer
	}
	for _, digest := range digests {
		have, err := session.HasLayer(ctx, digest)
		if err != nil {
			return fmt.Errorf("check layer %s: %w", digest, err)
		}
		if have {
			continue
		}
		layer, ok := blobs[digest]
		if !ok {
			return fmt.Errorf("no local blob for layer %s", digest)
		}
		if err := sendLayer(ctx, session, layer); err != nil {
			return fmt.Errorf("upload layer %s: %w", digest, err)
		}
	}
	return nil
}

func sendLayer(ctx context.Context, session *guestwire.Session, layer Layer) error {
	blob, err := os.Open(layer.Path)
	if err != nil {
		return err
	}
	defer blob.Close()
	info, err := blob.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("layer blob must be a regular file")
	}
	_, err = session.SendLayer(ctx, layer.Digest, layer.MediaType, blob, info.Size())
	return err
}
