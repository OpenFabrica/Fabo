//go:build linux || darwin

package localengine

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/bzip2"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/klauspost/compress/zstd"
	"github.com/ulikunitz/xz"

	"github.com/OpenFabrica/FabriBay/engine/internal/guestwire"
)

const maxArchiveJSON = 4 << 20
const plainLayer = "application/vnd.oci.image.layer.v1.tar"

type archiveManifestEntry struct {
	Config   string
	RepoTags []string
	Layers   []string
}

func imageFromConfig(raw []byte) (Image, v1.ConfigFile, error) {
	var config v1.ConfigFile
	if err := json.Unmarshal(raw, &config); err != nil {
		return Image{}, config, fmt.Errorf("read image config: %w", err)
	}
	image := Image{
		Architecture: config.Architecture, OS: config.OS, Variant: config.Variant,
		ExposedPorts: config.Config.ExposedPorts,
		ID:           fmt.Sprintf("sha256:%x", sha256.Sum256(raw)), Labels: config.Config.Labels, ConfigBlob: raw,
		Created: config.Created.Unix(), Entrypoint: config.Config.Entrypoint, Cmd: config.Config.Cmd,
		Env: config.Config.Env, WorkingDir: config.Config.WorkingDir, User: config.Config.User, StopSignal: config.Config.StopSignal,
	}
	for _, diff := range config.RootFS.DiffIDs {
		if diff.Algorithm != "sha256" || !validBlobName(diff.Hex) {
			return Image{}, config, fmt.Errorf("unsupported layer DiffID %q", diff.String())
		}
		image.DiffIDs = append(image.DiffIDs, diff.String())
	}
	return image, config, nil
}

type archiveReader struct {
	io.Reader
	close func()
}

func (r archiveReader) Close() error {
	r.close()
	return nil
}

func decodeArchive(input io.Reader, outer bool) (io.ReadCloser, string, error) {
	buf := bufio.NewReader(input)
	header, err := buf.Peek(6)
	if err != nil && err != io.EOF {
		return nil, "", err
	}
	switch {
	case bytes.HasPrefix(header, []byte{0x1f, 0x8b}):
		r, err := gzip.NewReader(buf)
		return r, plainLayer + "+gzip", err
	case bytes.HasPrefix(header, []byte{0x28, 0xb5, 0x2f, 0xfd}):
		r, err := zstd.NewReader(buf, zstd.WithDecoderMaxMemory(128<<20))
		if err != nil {
			return nil, "", err
		}
		return archiveReader{r, r.Close}, plainLayer + "+zstd", nil
	case outer && bytes.HasPrefix(header, []byte("BZh")):
		return io.NopCloser(bzip2.NewReader(buf)), "", nil
	case outer && bytes.HasPrefix(header, []byte{0xfd, '7', 'z', 'X', 'Z', 0}):
		r, err := (xz.ReaderConfig{DictCap: 128 << 20}).NewReader(buf)
		if err != nil {
			return nil, "", err
		}
		return io.NopCloser(r), "", nil
	default:
		return io.NopCloser(buf), plainLayer, nil
	}
}

type archiveContextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r archiveContextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}

// Read the complete decoded tar, including padding and the codec checksum,
// to verify the uncompressed digest recorded in the image config.
func archiveLayerDiffID(ctx context.Context, input io.Reader, size int64) (string, string, error) {
	decoded, media, err := decodeArchive(input, false)
	if err != nil {
		return "", "", err
	}
	defer decoded.Close()
	limited := &io.LimitedReader{R: archiveContextReader{ctx, decoded}, N: guestwire.LayerExpansionLimit(uint64(size)) + 1}
	hash := sha256.New()
	stream := io.TeeReader(limited, hash)
	layers := tar.NewReader(stream)
	for {
		_, err := layers.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", "", fmt.Errorf("read layer tar: %w", err)
		}
		if _, err := io.Copy(io.Discard, layers); err != nil {
			return "", "", err
		}
	}
	if _, err := io.Copy(io.Discard, stream); err != nil {
		return "", "", err
	}
	if limited.N == 0 {
		return "", "", fmt.Errorf("layer expands beyond its allowed size")
	}
	return fmt.Sprintf("sha256:%x", hash.Sum(nil)), media, nil
}
