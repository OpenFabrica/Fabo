//go:build linux || darwin

package localengine

import (
	"encoding/json"
	"fmt"
	"runtime"

	v1 "github.com/google/go-containerregistry/pkg/v1"
)

func nativePlatform() v1.Platform {
	return v1.Platform{OS: "linux", Architecture: runtime.GOARCH}
}

// platform recovers metadata from configs stored before these fields were
// indexed. Catalog entries without an image config describe native images.
func (i Image) platform() v1.Platform {
	p := v1.Platform{OS: i.OS, Architecture: i.Architecture, Variant: i.Variant}
	if p.Architecture == "" && len(i.ConfigBlob) != 0 {
		_ = json.Unmarshal(i.ConfigBlob, &p)
	}
	if p.OS == "" {
		p.OS = "linux"
	}
	if p.Architecture == "" {
		p.Architecture = runtime.GOARCH
	}
	return p
}

func storableImagePlatform(p v1.Platform) bool {
	return p.OS == "linux" && (p.Architecture == runtime.GOARCH || p.Architecture == "arm64" || p.Architecture == "amd64")
}

func parseImagePlatform(raw string) (v1.Platform, error) {
	p, err := v1.ParsePlatform(raw)
	if err != nil || !storableImagePlatform(*p) || p.OSVersion != "" || p.String() != raw {
		return v1.Platform{}, fmt.Errorf("%w: unsupported image platform %q", errInvalid, raw)
	}
	return *p, nil
}

func matchesImagePlatform(actual, wanted v1.Platform) bool {
	if actual.OS != wanted.OS || actual.Architecture != wanted.Architecture {
		return false
	}
	if wanted.Variant == "" || wanted.Variant == actual.Variant {
		return true
	}
	return actual.Variant == "" && (wanted.Architecture == "arm64" && wanted.Variant == "v8" ||
		wanted.Architecture == "amd64" && wanted.Variant == "v1")
}

func imagePlatforms(raw []string) ([]v1.Platform, error) {
	var platforms []v1.Platform
	for _, value := range raw {
		if value == "" {
			continue
		}
		p, err := parseImagePlatform(value)
		if err != nil {
			return nil, err
		}
		platforms = append(platforms, p)
	}
	return platforms, nil
}

func selectedPlatform(actual v1.Platform, wanted []v1.Platform) bool {
	for _, p := range wanted {
		if matchesImagePlatform(actual, p) {
			return true
		}
	}
	return len(wanted) == 0
}

func (e *Engine) checkExecutionPlatform(p v1.Platform) error {
	if p.OS == "linux" && p.Architecture == runtime.GOARCH {
		return nil
	}
	if p.OS == "linux" && p.Architecture == "amd64" && e.rosetta {
		return nil
	}
	if runtime.GOOS == "darwin" && runtime.GOARCH == "arm64" && p.OS == "linux" && p.Architecture == "amd64" {
		return fmt.Errorf("%w: linux/amd64 requires Rosetta for Linux in the Kiri VM", errInvalid)
	}
	return fmt.Errorf("%w: this engine cannot run %s images", errInvalid, p)
}
