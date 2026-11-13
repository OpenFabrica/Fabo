//go:build linux || darwin

package localengine

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/netip"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

// BuildMirror is the managed BuildKit worker's read-only view of tagged
// images. Its listener and private route survive guest sleep and wake.
type BuildMirror struct {
	host, address, token string
	engine               func() *Engine
	server               *http.Server
}

// OpenBuildMirror serves local image manifests and blobs to BuildKit.
// current supplies the daemon's current image store after a guest restart.
func OpenBuildMirror(ctx context.Context, current func() *Engine) (*BuildMirror, error) {
	listener, host, err := listenBuildMirror(ctx)
	if err != nil {
		return nil, err
	}
	_, port, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		_ = listener.Close()
		return nil, err
	}
	m := &BuildMirror{host: host, address: net.JoinHostPort(host, port), token: rand.Text(), engine: current}
	m.server = &http.Server{Handler: m, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 30 * time.Second,
		BaseContext: func(net.Listener) context.Context { return ctx }}
	go func() {
		if err := m.server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("serve local build images: %v", err)
		}
	}()
	return m, nil
}

func (m *BuildMirror) Close() error { return m.server.Close() }

func buildRegistryHost(host string) string {
	if host == name.DefaultRegistry {
		return "docker.io"
	}
	return host
}

func localRegistry(host string) bool {
	if hostname, _, err := net.SplitHostPort(host); err == nil {
		host = hostname
	}
	host = strings.Trim(strings.ToLower(host), "[]")
	ip, _ := netip.ParseAddr(host)
	return host == "localhost" || strings.HasSuffix(host, ".localhost") || ip.IsLoopback()
}

func (m *BuildMirror) config(e *Engine) string {
	if m == nil {
		return ""
	}
	registries := []string{"docker.io"}
	e.mu.Lock()
	for ref := range e.images {
		if parsed, err := name.ParseReference(ref); err == nil {
			host := buildRegistryHost(parsed.Context().RegistryStr())
			if !localRegistry(host) && !slices.Contains(registries, host) {
				registries = append(registries, host)
			}
		}
	}
	e.mu.Unlock()
	slices.Sort(registries)
	var config strings.Builder
	for _, registry := range registries {
		fmt.Fprintf(&config, "\n[registry.%q]\n  mirrors = [%q]\n", registry, m.address+"/"+m.token+"/"+registry)
	}
	fmt.Fprintf(&config, "\n[registry.%q]\n  http = true\n", m.address)
	return config.String()
}

// Keep the private endpoint out of any proxy injected into the managed
// builder. Other processes retain their configured proxy environment.
func (m *BuildMirror) environment(env []string) []string {
	if m == nil {
		return env
	}
	values := make([]string, 0, 2)
	for _, key := range []string{"NO_PROXY", "no_proxy"} {
		value := m.host
		for _, entry := range env {
			if list, ok := strings.CutPrefix(entry, key+"="); ok && list != "" {
				value = list + "," + m.host
				break
			}
		}
		values = append(values, key+"="+value)
	}
	return mergeEnv(env, values)
}

type mirrorRequest struct {
	repo     name.Repository
	ref      string
	manifest bool
}

func (m *BuildMirror) parse(path string) (mirrorRequest, bool) {
	rest, ok := strings.CutPrefix(path, "/v2/")
	token, rest, separated := strings.Cut(rest, "/")
	if !ok || !separated || subtle.ConstantTimeCompare([]byte(token), []byte(m.token)) != 1 {
		return mirrorRequest{}, false
	}
	registry, rest, ok := strings.Cut(rest, "/")
	if !ok || registry == "" {
		return mirrorRequest{}, false
	}
	i := strings.LastIndexByte(rest, '/')
	if i < 0 || i == len(rest)-1 {
		return mirrorRequest{}, false
	}
	base, ref := rest[:i], rest[i+1:]
	// The resource segment is taken from the end, since repositories
	// may themselves include a path component named manifests or blobs.
	j := strings.LastIndexByte(base, '/')
	if j <= 0 {
		return mirrorRequest{}, false
	}
	repoPath, resource := base[:j], base[j+1:]
	if resource != "manifests" && resource != "blobs" {
		return mirrorRequest{}, false
	}
	repo, err := name.NewRepository(registry + "/" + repoPath)
	if err != nil || buildRegistryHost(repo.RegistryStr()) != buildRegistryHost(registry) {
		return mirrorRequest{}, false
	}
	return mirrorRequest{repo, ref, resource == "manifests"}, true
}

func (m *BuildMirror) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	req, ok := m.parse(r.URL.Path)
	if !ok {
		http.NotFound(w, r)
		return
	}
	e := m.engine()
	if e != nil {
		if req.manifest {
			if raw, digest := e.mirrorManifest(r.Context(), req); raw != nil {
				serveMirrorContent(w, r, digest, string(types.OCIManifestSchema1), bytes.NewReader(raw))
				return
			}
		} else if e.serveMirrorBlob(w, r, req) {
			return
		}
	}
	// A miss lets BuildKit try the original registry.
	http.NotFound(w, r)
}

func (e *Engine) mirrorImages(repo name.Repository) []Image {
	e.mu.Lock()
	defer e.mu.Unlock()
	var images []Image
	seen := make(map[string]bool)
	for ref, image := range e.images {
		if parsed, err := name.ParseReference(ref); err == nil && parsed.Context().Name() == repo.Name() && !seen[image.ID] {
			images = append(images, image)
			seen[image.ID] = true
		}
	}
	return images
}

func (e *Engine) mirrorManifest(ctx context.Context, req mirrorRequest) ([]byte, string) {
	var images []Image
	pinned := strings.HasPrefix(req.ref, "sha256:")
	if pinned {
		images = e.mirrorImages(req.repo)
	} else {
		if _, err := name.NewTag(req.repo.Name() + ":" + req.ref); err != nil {
			return nil, ""
		}
		e.mu.Lock()
		_, image, found := e.imageReference(req.repo.Name() + ":" + req.ref)
		e.mu.Unlock()
		if found {
			images = []Image{image}
		}
	}
	for _, image := range images {
		snapshot, err := openImageLayers([]exportImage{{Image: image}})
		if err != nil {
			continue
		}
		payload, err := snapshot.registryImage(ctx, image)
		snapshot.close()
		if err != nil {
			continue
		}
		digest := fmt.Sprintf("sha256:%x", sha256.Sum256(payload.manifest))
		if !pinned || digest == req.ref {
			return payload.manifest, digest
		}
	}
	return nil, ""
}

func (e *Engine) serveMirrorBlob(w http.ResponseWriter, r *http.Request, req mirrorRequest) bool {
	hex, sha := strings.CutPrefix(req.ref, "sha256:")
	if !sha || !validBlobName(hex) {
		return false
	}
	for _, image := range e.mirrorImages(req.repo) {
		if image.ID == req.ref && len(image.ConfigBlob) != 0 {
			serveMirrorContent(w, r, req.ref, "application/octet-stream", bytes.NewReader(image.ConfigBlob))
			return true
		}
		for _, layer := range image.Layers {
			if layer.Digest != req.ref {
				continue
			}
			file, err := os.Open(layer.Path)
			if err != nil {
				continue
			}
			info, err := file.Stat()
			if err == nil && info.Mode().IsRegular() {
				serveMirrorContent(w, r, req.ref, "application/octet-stream", io.NewSectionReader(file, 0, info.Size()))
				_ = file.Close()
				return true
			}
			_ = file.Close()
		}
		if len(image.ConfigBlob) == 0 {
			// Older records reconstruct their config during archive export.
			snapshot, err := openImageLayers([]exportImage{{Image: image}})
			if err == nil {
				raw, err := snapshot.config(r.Context(), image)
				snapshot.close()
				if err == nil && fmt.Sprintf("sha256:%x", sha256.Sum256(raw)) == req.ref {
					serveMirrorContent(w, r, req.ref, "application/octet-stream", bytes.NewReader(raw))
					return true
				}
			}
		}
	}
	return false
}

func serveMirrorContent(w http.ResponseWriter, r *http.Request, digest, contentType string, data io.ReadSeeker) {
	w.Header().Set("Docker-Content-Digest", digest)
	w.Header().Set("Content-Type", contentType)
	http.ServeContent(w, r, "", time.Time{}, data)
}
