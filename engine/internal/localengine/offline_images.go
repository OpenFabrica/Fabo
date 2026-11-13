//go:build linux || darwin

package localengine

import (
	"context"
	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"slices"
)

// OfflineImages reuses the registry cache and index for a raw guest's private
// store, without opening a Docker API server or restoring daemon workloads.
// The caller owns State's exclusive lock and serializes these operations.
type OfflineImages struct{ engine *Engine }

func OpenOfflineImages(ctx context.Context, state *State) (*OfflineImages, error) {
	index, err := state.restoreImages()
	if err != nil {
		return nil, err
	}
	e := &Engine{ctx: ctx, state: state, imageIndex: index, catalog: map[string]Image{}, images: index.references(nil), containers: map[string]*container{}}
	return &OfflineImages{e}, nil
}
func (s *OfflineImages) Pull(ctx context.Context, reference string) (Image, error) {
	ref, e := name.ParseReference(reference)
	if e != nil {
		return Image{}, e
	}
	return s.engine.pull(ctx, ref, authn.AuthConfig{})
}
func (s *OfflineImages) Find(reference string) (Image, error) {
	image, e := s.engine.findImage(reference)
	return image.Image, e
}

type OfflineImage struct {
	Name  string
	Image Image
}

func (s *OfflineImages) List() []OfflineImage {
	out := []OfflineImage{}
	for name, image := range s.engine.images {
		out = append(out, OfflineImage{name, image})
	}
	slices.SortFunc(out, func(a, b OfflineImage) int {
		if a.Name < b.Name {
			return -1
		}
		if a.Name > b.Name {
			return 1
		}
		return 0
	})
	return out
}
func (s *OfflineImages) Remove(reference string) error {
	if _, e := s.engine.removeImage(reference, false); e != nil {
		return e
	}
	_, e := s.engine.collectUnusedLayers()
	return e
}
