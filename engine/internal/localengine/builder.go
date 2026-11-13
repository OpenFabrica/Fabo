//go:build linux || darwin

package localengine

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
)

const defaultManagedBuilder = "buildx_buildkit_kiri-build0"

const managedBuilderImage = "moby/buildkit:buildx-stable-1"

// This is the same multi-platform image index pinned by Kiri's Rust builder.
const managedBuilderPin = "moby/buildkit@sha256:cec9f139f45e93c5c69c60f8b07cfad9f43f4ef6b6a6cd917527fea5ff2e3dea"

func (e *Engine) defaultBuilder() string {
	if e.builderName != "" {
		return e.builderName
	}
	return defaultManagedBuilder
}

func (e *Engine) managedBuilderName(raw string) string {
	name := strings.TrimPrefix(raw, "/")
	if e.builderName != "" {
		if name == e.builderName {
			return name
		}
		return ""
	}
	switch name {
	case defaultManagedBuilder, "buildx_buildkit_kiri-dev-build0":
		return name
	default:
		return ""
	}
}

// createManagedBuilder owns the same named container and state volume as
// Kiri's buildx docker-container driver. The buildx client starts it.
func (e *Engine) createManagedBuilder(ctx context.Context, builder string) (string, error) {
	e.builderOps.Lock()
	defer e.builderOps.Unlock()
	return e.newManagedBuilder(ctx, builder)
}

func (e *Engine) checkBuilderRuntime() error {
	if e.remoteGuest && !e.guestDisk {
		return fmt.Errorf("%w: the managed builder requires a Kiri VM data disk", errInvalid)
	}
	if !e.remoteGuest && !e.hostCgroups {
		return fmt.Errorf("%w: the managed builder requires delegated cgroups on Linux", errInvalid)
	}
	if e.guestArchive == nil {
		return fmt.Errorf("%w: the managed builder requires the guest archive service", errInvalid)
	}
	return nil
}

// newManagedBuilder requires builderOps.
func (e *Engine) newManagedBuilder(ctx context.Context, builder string) (string, error) {
	if err := e.checkBuilderRuntime(); err != nil {
		return "", err
	}
	if err := e.ensureBuilderImage(ctx); err != nil {
		return "", err
	}
	cfg := Config{
		Image:      managedBuilderImage,
		Cmd:        []string{"--allow-insecure-entitlement=network.host"},
		Labels:     map[string]string{"dev.kiri.builder": "1"},
		volumes:    []volumeMount{{name: builder + "_state", target: "/var/lib/buildkit"}},
		privileged: true, bridge: true,
	}
	return e.create(builder, cfg)
}

func (e *Engine) ensureBuilderImage(ctx context.Context) error {
	e.mu.Lock()
	_, _, have := e.imageReference(managedBuilderImage)
	e.mu.Unlock()
	if have {
		return nil
	}
	ref, err := name.NewDigest(managedBuilderPin)
	if err != nil {
		return err
	}
	image, err := e.pull(ctx, ref, authn.AuthConfig{})
	if err != nil {
		return fmt.Errorf("pull Kiri BuildKit image: %w", err)
	}
	tag, err := name.NewTag(managedBuilderImage)
	if err != nil {
		return err
	}
	e.imageOps.Lock()
	defer e.imageOps.Unlock()
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return fmt.Errorf("daemon is shutting down")
	}
	if _, _, have := e.imageReference(managedBuilderImage); have {
		return nil
	}
	if err := e.putImage(tag.Name(), image); err != nil {
		return err
	}
	e.emitImage(image, "tag", tag.Name())
	return nil
}
