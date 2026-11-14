//go:build linux || darwin

package localengine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/OpenFabrica/FabriBay/engine/internal/guestwire"
	"golang.org/x/sys/unix"
)

const (
	builderSettingsFile = "builder.json"
	builderGiB          = uint64(1 << 30)
)

type builderSettings struct {
	CacheGB  *uint64 `json:"cache_gb"`
	IdleSecs *uint64 `json:"idle_secs,omitempty"`
}

func (b *builderSettings) validate() error {
	if b.CacheGB != nil {
		if *b.CacheGB > 100_000 {
			return fmt.Errorf("%w: cache_gb must be a whole number of GB up to 100000", errInvalid)
		}
		if *b.CacheGB == 0 {
			b.CacheGB = nil
		}
	}
	if b.IdleSecs != nil && *b.IdleSecs > 7*86400 {
		return fmt.Errorf("%w: idle_secs must be 0 to 604800", errInvalid)
	}
	return nil
}

func (s *State) loadBuilderSettings() (builderSettings, error) {
	var settings builderSettings
	data, err := os.ReadFile(filepath.Join(s.dir, builderSettingsFile))
	if errors.Is(err, os.ErrNotExist) {
		return settings, nil
	}
	if err == nil {
		err = json.Unmarshal(data, &settings)
	}
	if err == nil {
		err = settings.validate()
	}
	if err != nil {
		return builderSettings{}, fmt.Errorf("read builder settings: %v", err)
	}
	return settings, nil
}

func (s *State) updateBuilderSettings(cache json.RawMessage, idle *uint64) (builderSettings, error) {
	s.builderSettingsMu.Lock()
	defer s.builderSettingsMu.Unlock()
	settings, err := s.loadBuilderSettings()
	if err != nil {
		return builderSettings{}, err
	}
	if len(cache) != 0 {
		var limit *uint64
		if err := json.Unmarshal(cache, &limit); err != nil {
			return builderSettings{}, fmt.Errorf("%w: cache_gb must be a whole number of GB", errInvalid)
		}
		settings.CacheGB = limit
	}
	if idle != nil {
		settings.IdleSecs = idle
	}
	if err := settings.validate(); err != nil {
		return builderSettings{}, err
	}
	return settings, saveJSON(s.dir, builderSettingsFile, settings)
}

func builderIdleSeconds(settings builderSettings) uint64 {
	if idle, err := strconv.ParseUint(os.Getenv("FABO_BUILDER_IDLE_SECS"), 10, 64); err == nil && idle <= math.MaxInt64/uint64(time.Second) {
		return idle
	}
	if settings.IdleSecs != nil {
		return *settings.IdleSecs
	}
	return 300
}

func defaultBuilderCache(dir string) uint64 {
	const ceiling = 20 * builderGiB
	var stats unix.Statfs_t
	if err := unix.Statfs(dir, &stats); err != nil || stats.Bsize <= 0 {
		return ceiling
	}
	block := uint64(stats.Bsize)
	if stats.Blocks > math.MaxUint64/block {
		return ceiling
	}
	return min(ceiling, max(builderGiB, stats.Blocks*block/10))
}

func builderCacheLimit(settings builderSettings, fallback uint64) uint64 {
	if gb, err := strconv.ParseUint(os.Getenv("FABO_BUILD_CACHE_GB"), 10, 64); err == nil && gb <= math.MaxUint64/builderGiB {
		if gb == 0 {
			return fallback
		}
		return gb * builderGiB
	}
	if settings.CacheGB != nil {
		return *settings.CacheGB * builderGiB
	}
	return fallback
}

func (e *Engine) builderSettingsHTTP(w http.ResponseWriter, r *http.Request) {
	var settings builderSettings
	var err error
	if r.Method == http.MethodPost {
		var body struct {
			CacheGB  json.RawMessage `json:"cache_gb"`
			IdleSecs *uint64         `json:"idle_secs"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&body); err != nil {
			writeError(w, fmt.Errorf("%w: invalid builder settings: %v", errInvalid, err))
			return
		}
		settings, err = e.state.updateBuilderSettings(body.CacheGB, body.IdleSecs)
	} else {
		settings, err = e.state.loadBuilderSettings()
	}
	if err != nil {
		writeError(w, err)
		return
	}
	name, state := e.defaultBuilder(), "none"
	e.mu.Lock()
	if c, err := e.find(name); err == nil {
		state = c.state
	}
	e.mu.Unlock()
	_, cacheOverride := os.LookupEnv("FABO_BUILD_CACHE_GB")
	_, idleOverride := os.LookupEnv("FABO_BUILDER_IDLE_SECS")
	defaultCache := defaultBuilderCache(e.state.dir)
	writeJSON(w, http.StatusOK, map[string]any{
		"cache_gb": settings.CacheGB, "default_cache_gb": defaultCache / builderGiB,
		"cache_limit": builderCacheLimit(settings, defaultCache), "idle_secs": builderIdleSeconds(settings),
		"image": managedBuilderImage, "container": name, "volume": name + "_state", "state": state,
		"overridden": cacheOverride || idleOverride,
	})
}

// writeBuilderConfig is called with the container operation held, before
// starting the managed BuildKit daemon on the saved writable layer.
func (e *Engine) writeBuilderConfig(c *container) error {
	settings, err := e.state.loadBuilderSettings()
	if err != nil {
		return err
	}
	limit := builderCacheLimit(settings, defaultBuilderCache(e.state.dir))
	config := fmt.Sprintf("# Managed by Fabo\n[worker.oci]\n  gc = true\n  maxUsedSpace = %d\n  reservedSpace = %d\n", limit, limit/4)
	config += e.buildMirror.config(e)
	archive, err := buildFileArchive("buildkit/buildkitd.toml", 0644, []byte(config))
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(e.ctx, 2*time.Minute)
	defer cancel()
	conn, closeArchive, err := e.openContainerArchive(ctx, c, guestwire.CopySpec{
		Op: guestwire.CopyIn, Path: "/etc", Flags: guestwire.CopyChunked | guestwire.CopyNoReplace,
	})
	if err != nil {
		return fmt.Errorf("open BuildKit configuration archive: %w", err)
	}
	defer closeArchive()
	if err := uploadContainerArchive(conn, archive); err != nil {
		return fmt.Errorf("write BuildKit configuration: %w", err)
	}
	return nil
}
