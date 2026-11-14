//go:build linux || darwin

package localengine

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/netip"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/name"

	"github.com/OpenFabrica/Fabo/engine/internal/guestwire"
	"github.com/OpenFabrica/Fabo/engine/internal/hostagent"
)

type kiriImage struct {
	Name, Digest string
	IndexDigest  string `json:"index_digest"`
	ConfigDigest string `json:"config_digest"`
	Layers       []struct {
		Digest    string
		Size      int64
		MediaType string `json:"media_type"`
	}
}

func kiriDigest(digest string) (string, error) {
	hex, ok := strings.CutPrefix(digest, "sha256:")
	if !ok || !validBlobName(hex) {
		return "", fmt.Errorf("invalid SHA-256 digest %q", digest)
	}
	return hex, nil
}

func migrateKiriImages(ctx context.Context, source *os.Root, stage, dest string) (imageIndex, error) {
	index := imageIndex{}.clone()
	records, err := readKiriIndex[kiriImage](source, "images.json")
	if err != nil {
		return index, err
	}
	if err := os.Mkdir(filepath.Join(stage, "blobs"), 0700); err != nil {
		return index, err
	}
	blobs := make(map[string]int64)
	for _, record := range records {
		if err := ctx.Err(); err != nil {
			return index, err
		}
		config, err := kiriDigest(record.ConfigDigest)
		if err != nil {
			return index, err
		}
		file, err := source.Open(path.Join("blobs/sha256", config))
		if err != nil {
			return index, err
		}
		raw, err := io.ReadAll(io.LimitReader(file, maxArchiveJSON+1))
		closeErr := file.Close()
		if err != nil {
			return index, err
		}
		if closeErr != nil {
			return index, closeErr
		}
		if len(raw) > maxArchiveJSON {
			return index, fmt.Errorf("image %s config is too large", record.Name)
		}
		image, _, err := imageFromConfig(raw)
		if err != nil {
			return index, err
		}
		if image.ID != record.ConfigDigest || len(image.DiffIDs) != len(record.Layers) ||
			!storableImagePlatform(image.platform()) {
			return index, fmt.Errorf("image %s has an incompatible platform, config or layer count", record.Name)
		}
		for _, layer := range record.Layers {
			hex, err := kiriDigest(layer.Digest)
			if err != nil {
				return index, err
			}
			switch layer.MediaType {
			case plainLayer, plainLayer + "+gzip", plainLayer + "+zstd",
				"application/vnd.docker.image.rootfs.diff.tar", "application/vnd.docker.image.rootfs.diff.tar.gzip":
			default:
				return index, fmt.Errorf("image %s has unsupported layer media type %q", record.Name, layer.MediaType)
			}
			if layer.Size < 0 || layer.Size > math.MaxInt64-image.Size {
				return index, fmt.Errorf("invalid image layer size for %s", record.Name)
			}
			if size, exists := blobs[hex]; exists {
				if size != layer.Size {
					return index, fmt.Errorf("inconsistent size for layer %s", layer.Digest)
				}
			} else {
				if err := copyKiriLayer(ctx, source, stage, hex, layer.Size); err != nil {
					return index, err
				}
				blobs[hex] = layer.Size
			}
			image.Size += layer.Size
			image.Layers = append(image.Layers, hostagent.Layer{
				Digest: layer.Digest, MediaType: layer.MediaType, Path: filepath.Join(dest, "blobs", hex),
			})
		}
		if record.Name == image.ID {
			index.Dangling[image.ID] = image
			continue
		}
		ref, err := name.ParseReference(record.Name)
		if err != nil {
			return index, fmt.Errorf("invalid source image reference %q: %w", record.Name, err)
		}
		digest := record.IndexDigest
		if digest == "" {
			digest = record.Digest
		}
		if digest != "" && digest != image.ID {
			if _, err := kiriDigest(digest); err != nil {
				return index, err
			}
			image.RepoDigest = ref.Context().Name() + "@" + digest
		}
		if previous := index.Overrides[ref.Name()]; previous != nil && previous.ID != image.ID {
			return index, fmt.Errorf("conflicting source image reference %s", ref.Name())
		}
		index.Overrides[ref.Name()] = &image
	}
	for _, image := range index.Overrides {
		delete(index.Dangling, image.ID)
	}
	return index, nil
}

func copyKiriLayer(ctx context.Context, source *os.Root, stage, hex string, expectedSize int64) error {
	dest := filepath.Join(stage, "blobs", hex)
	if err := copyKiriFile(ctx, source, path.Join("blobs/sha256", hex), dest); err != nil {
		return fmt.Errorf("copy image layer %s: %w", hex, err)
	}
	file, err := os.Open(dest)
	if err != nil {
		return err
	}
	defer file.Close()
	hash := sha256.New()
	size, err := io.Copy(hash, archiveContextReader{ctx, file})
	if err != nil {
		return err
	}
	if size != expectedSize || fmt.Sprintf("%x", hash.Sum(nil)) != hex {
		return fmt.Errorf("source layer sha256:%s does not match its record", hex)
	}
	return nil
}

type kiriVolume struct {
	Name            string
	CreatedNS       int64 `json:"created_ns"`
	Anonymous       bool
	Labels, Options map[string]string
}

func migrateKiriVolumes(ctx context.Context, source *os.Root, state *State) (int, error) {
	records, err := readKiriIndex[kiriVolume](source, "volumes.json")
	if err != nil {
		return 0, err
	}
	for _, entry := range records {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		if !guestwire.ValidVolumeName(entry.Name) {
			return 0, fmt.Errorf("volume %q has an unsupported name", entry.Name)
		}
		if _, err := localVolumeMount(entry.Options); err != nil {
			return 0, fmt.Errorf("volume %q has invalid local driver options: %w", entry.Name, err)
		}
		dir := filepath.Join(state.dir, "volumes", entry.Name)
		if err := os.MkdirAll(filepath.Dir(dir), 0700); err != nil {
			return 0, err
		}
		if err := os.Mkdir(dir, 0700); err != nil {
			return 0, err
		}
		v := volume{Name: entry.Name, Anonymous: entry.Anonymous, Driver: "local", Scope: "local", Labels: entry.Labels,
			CreatedAt: time.Unix(0, entry.CreatedNS).UTC().Format(time.RFC3339Nano), Options: entry.Options}
		if err := saveJSON(dir, "volume.json", v); err != nil {
			return 0, err
		}
	}
	return len(records), nil
}

type kiriNetwork struct {
	ID, Name, Driver     string
	Subnet, Gateway      [4]byte
	Prefix               uint8
	CreatedNS            int64 `json:"created_ns"`
	Internal, Attachable bool
	Labels, Options      map[string]string
	IPAMOptions          map[string]string `json:"ipam_options"`
	IPRange              json.RawMessage   `json:"ip_range"`
	Aux                  map[string][4]byte
	NoIPv4               bool `json:"no_ipv4"`
	V6                   *kiriNetworkV6
}

type kiriNetworkV6 struct {
	Subnet, Gateway [16]byte
	Prefix          uint8
	IPRange         json.RawMessage `json:"ip_range"`
	Aux             map[string][16]byte
}

func (v kiriNetworkV6) pool() (*ipv6Pool, error) {
	pool := &ipv6Pool{
		Subnet:  netip.PrefixFrom(netip.AddrFrom16(v.Subnet), int(v.Prefix)),
		Gateway: netip.AddrFrom16(v.Gateway),
	}
	if hasKiriValue(v.IPRange) {
		var tuple []json.RawMessage
		var address [16]byte
		var bits uint8
		if err := json.Unmarshal(v.IPRange, &tuple); err != nil || len(tuple) != 2 ||
			json.Unmarshal(tuple[0], &address) != nil || json.Unmarshal(tuple[1], &bits) != nil || bits > 128 {
			return nil, fmt.Errorf("invalid IPv6 allocation range")
		}
		pool.IPRange = netip.PrefixFrom(netip.AddrFrom16(address), int(bits))
	}
	if len(v.Aux) != 0 {
		pool.Auxiliary = make(map[string]netip.Addr, len(v.Aux))
		for name, address := range v.Aux {
			pool.Auxiliary[name] = netip.AddrFrom16(address)
		}
	}
	if !pool.valid() {
		return nil, fmt.Errorf("invalid IPv6 subnet, gateway or allocation range")
	}
	return pool, nil
}

func migrateKiriNetworks(source *os.Root, state *State) (map[string]dockerNetwork, error) {
	records, err := readKiriIndex[kiriNetwork](source, "networks.json")
	if err != nil {
		return nil, err
	}
	networks := make(map[string]dockerNetwork, len(records))
	for _, entry := range records {
		if entry.Driver != "bridge" || !validBlobName(entry.ID) {
			return nil, fmt.Errorf("network %q uses an unsupported driver or invalid ID", entry.Name)
		}
		if _, duplicate := networks[entry.Name]; duplicate {
			return nil, fmt.Errorf("duplicate source network %q", entry.Name)
		}
		n := dockerNetwork{
			ID: entry.ID, Name: entry.Name, Bridge: "fabo" + entry.ID[:10],
			NoIPv4:   entry.NoIPv4,
			Internal: entry.Internal, Attachable: entry.Attachable, Created: time.Unix(0, entry.CreatedNS).UTC(), Labels: entry.Labels,
			Options: entry.Options, IPAMOptions: entry.IPAMOptions,
		}
		if !n.NoIPv4 {
			n.Subnet = netip.PrefixFrom(netip.AddrFrom4(entry.Subnet), int(entry.Prefix))
			n.Gateway = netip.AddrFrom4(entry.Gateway)
		}
		if hasKiriValue(entry.IPRange) {
			var tuple []json.RawMessage
			var address [4]byte
			var bits uint8
			if err := json.Unmarshal(entry.IPRange, &tuple); err != nil || len(tuple) != 2 ||
				json.Unmarshal(tuple[0], &address) != nil || json.Unmarshal(tuple[1], &bits) != nil || bits > 32 {
				return nil, fmt.Errorf("network %q: invalid IPv4 allocation range", entry.Name)
			}
			n.IPRange = netip.PrefixFrom(netip.AddrFrom4(address), int(bits))
		}
		if len(entry.Aux) != 0 {
			n.Auxiliary = make(map[string]netip.Addr, len(entry.Aux))
			for name, address := range entry.Aux {
				n.Auxiliary[name] = netip.AddrFrom4(address)
			}
		}
		if entry.V6 != nil {
			n.IPv6, err = entry.V6.pool()
			if err != nil {
				return nil, fmt.Errorf("network %q: %w", entry.Name, err)
			}
		}
		networks[entry.Name] = n
	}
	if err := saveJSON(state.dir, "networks.json", networks); err != nil {
		return nil, err
	}
	return state.restoreNetworks()
}

func hasKiriValue(raw json.RawMessage) bool {
	switch strings.TrimSpace(string(raw)) {
	case "", "null", "false", "0", "[]", "{}":
		return false
	default:
		return true
	}
}
