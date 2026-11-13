//go:build linux || darwin

package localengine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	v1 "github.com/google/go-containerregistry/pkg/v1"

	"github.com/libracoder/kiri/engine/internal/imagescan"
)

type imageLayerRow struct {
	By      string  `json:"by"`
	Size    int64   `json:"size"`
	Digest  *string `json:"digest"`
	Empty   bool    `json:"empty"`
	Created string  `json:"created"`
	Base    bool    `json:"base"`
}

func (e *Engine) scannedImage(r *http.Request) (Image, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	image, err := e.findImage(r.URL.Query().Get("name"))
	return image.Image, err
}

func imageScanMetadata(image Image) (v1.ConfigFile, []imageLayerRow, []imagescan.Layer, error) {
	var config v1.ConfigFile
	if len(image.ConfigBlob) != 0 {
		if err := json.Unmarshal(image.ConfigBlob, &config); err != nil {
			return config, nil, nil, err
		}
	} else {
		config.Config = v1.Config{
			Cmd: image.Cmd, Entrypoint: image.Entrypoint, WorkingDir: image.WorkingDir, User: image.User,
			Env: image.Env, ExposedPorts: image.ExposedPorts, Labels: image.Labels, StopSignal: image.StopSignal,
		}
	}
	layers := make([]imageLayerRow, 0, max(len(config.History), len(image.Layers)))
	filesystems := make([]imagescan.Layer, 0, len(image.Layers))
	appendLayer := func(row imageLayerRow) {
		if !row.Empty && len(filesystems) < len(image.Layers) {
			i := len(filesystems)
			layer := image.Layers[i]
			row.Digest = &layer.Digest
			if info, err := os.Stat(layer.Path); err == nil {
				row.Size = info.Size()
			}
			attribution := imagescan.Layer{Digest: layer.Digest, By: row.By, Base: row.Base}
			if i < len(image.DiffIDs) {
				attribution.DiffID = image.DiffIDs[i]
			}
			filesystems = append(filesystems, attribution)
		}
		layers = append(layers, row)
	}
	inBase := !config.Created.IsZero()
	for _, h := range config.History {
		inBase = inBase && h.Created.Before(config.Created.Add(-time.Hour))
		row := imageLayerRow{By: strings.TrimSuffix(h.CreatedBy, " # buildkit"), Empty: h.EmptyLayer, Base: inBase}
		if !h.Created.IsZero() {
			row.Created = h.Created.Format(time.RFC3339Nano)
		}
		appendLayer(row)
	}
	for len(filesystems) < len(image.Layers) {
		appendLayer(imageLayerRow{})
	}
	return config, layers, filesystems, nil
}

func (e *Engine) imageLayersHTTP(w http.ResponseWriter, r *http.Request) {
	image, err := e.scannedImage(r)
	if err != nil {
		writeError(w, err)
		return
	}
	cfg, layers, _, err := imageScanMetadata(image)
	if err != nil {
		writeError(w, err)
		return
	}
	c := cfg.Config
	var health []string
	if c.Healthcheck != nil {
		health = c.Healthcheck.Test
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"name": r.URL.Query().Get("name"), "materials": []any{},
		"images": []any{map[string]any{
			"platform": image.platform().String(), "size": image.Size, "config_digest": image.ID, "layers": layers,
			"config": map[string]any{
				"cmd": c.Cmd, "entrypoint": c.Entrypoint, "workdir": c.WorkingDir, "user": c.User, "env": c.Env,
				"ports": slices.Sorted(maps.Keys(c.ExposedPorts)), "labels": c.Labels, "volumes": slices.Sorted(maps.Keys(c.Volumes)),
				"healthcheck": health, "stopsignal": c.StopSignal,
			},
		}},
	})
}

func (e *Engine) imageScanHTTP(w http.ResponseWriter, r *http.Request) {
	image, err := e.scannedImage(r)
	if err != nil {
		writeError(w, err)
		return
	}
	_, _, layers, err := imageScanMetadata(image)
	if err != nil {
		writeError(w, err)
		return
	}
	if r.Method == http.MethodGet {
		at, err := scanTime(r)
		if err == nil {
			var result *imagescan.Result
			result, err = e.state.scanner.Open(image.ID, at, layers)
			if err == nil {
				writeJSON(w, http.StatusOK, result)
				return
			}
		}
		writeError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	out := json.NewEncoder(w)
	stream := func(value any) error {
		if err := out.Encode(value); err != nil {
			return err
		}
		return http.NewResponseController(w).Flush()
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Minute)
	defer cancel()
	client := &http.Client{Transport: e.registryTransport(), Timeout: 5 * time.Minute}
	result, err := e.state.scanner.Scan(ctx, image.ID, layers, func(dir string) error {
		return e.writeScanLayout(ctx, image, dir)
	}, client, e.scanProxyEnv(), func(phase string, pct int) error {
		return stream(map[string]any{"phase": phase, "pct": pct})
	})
	if err != nil {
		_ = stream(map[string]string{"error": err.Error()})
		return
	}
	_ = stream(map[string]any{"result": result})
}

func (e *Engine) imageScansHTTP(w http.ResponseWriter, r *http.Request) {
	image, err := e.scannedImage(r)
	if err != nil {
		writeError(w, err)
		return
	}
	_, _, layers, err := imageScanMetadata(image)
	if err != nil {
		writeError(w, err)
		return
	}
	history, err := e.state.scanner.History(image.ID, layers)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": image.ID, "scans": history})
}

func (e *Engine) imageSBOMHTTP(w http.ResponseWriter, r *http.Request) {
	image, err := e.scannedImage(r)
	if err != nil {
		writeError(w, err)
		return
	}
	at, err := scanTime(r)
	if err != nil {
		writeError(w, err)
		return
	}
	if at == nil {
		writeError(w, fmt.Errorf("%w: expected a scan timestamp", errInvalid))
		return
	}
	f, err := e.state.scanner.SBOM(image.ID, *at)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			err = fmt.Errorf("%w: scan the image first", errMissing)
		}
		writeError(w, err)
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "application/json")
	http.ServeContent(w, r, "sbom.cdx.json", time.Time{}, f)
}

func scanTime(r *http.Request) (*int64, error) {
	raw := r.URL.Query().Get("at")
	if raw == "" {
		return nil, nil
	}
	at, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || at < 0 {
		return nil, fmt.Errorf("%w: invalid scan timestamp", errInvalid)
	}
	return &at, nil
}

func (e *Engine) scanProxyEnv() []string {
	if e.proxy == nil {
		return nil
	}
	_, p, relay := e.proxy.effective()
	proxyURL := func(value string) string {
		if value == "" {
			return ""
		}
		if u, err := url.Parse(value); err == nil {
			if port := relay[windowsProxyPort(u)]; port != 0 {
				u.Host = net.JoinHostPort("127.0.0.1", strconv.Itoa(int(port)))
				return u.String()
			}
		}
		return value
	}
	httpProxy, httpsProxy := proxyURL(p.HTTP), proxyURL(p.HTTPS)
	if httpsProxy == "" {
		httpsProxy = httpProxy
	}
	noProxy := strings.Join(p.NoProxy, ",")
	return []string{"HTTP_PROXY=" + httpProxy, "HTTPS_PROXY=" + httpsProxy, "NO_PROXY=" + noProxy,
		"http_proxy=" + httpProxy, "https_proxy=" + httpsProxy, "no_proxy=" + noProxy}
}
