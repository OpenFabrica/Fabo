//go:build linux || darwin

package localengine

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/OpenFabrica/FabriBay/engine/internal/diskusage"
	"github.com/OpenFabrica/FabriBay/engine/internal/hostagent"
)

type diskUsage struct {
	LayersSize uint64
	Images     []map[string]any
	Containers []map[string]any
	Volumes    []volumeUsage
	BuildCache []buildCacheUsage
}

type volumeUsage struct {
	volume
	UsageData struct {
		Size     int64
		RefCount int
	}
}

func (e *Engine) diskUsageHTTP(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	result, err := e.measureDiskUsage(ctx)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (e *Engine) measureDiskUsage(ctx context.Context) (diskUsage, error) {
	e.mu.Lock()
	images := e.allImages()
	imageTags, imageDigests := make(map[string][]string), make(map[string][]string)
	for id := range images {
		imageTags[id], imageDigests[id] = e.imageNames(id)
	}
	imageRefs, volumeRefs := make(map[string]int), make(map[string]int)
	containers := make([]map[string]any, 0, len(e.containers))
	containerLayers := make(map[string][]hostagent.Layer)
	for _, id := range slices.Sorted(maps.Keys(e.containers)) {
		c := e.containers[id]
		containers = append(containers, e.containerSummary(c))
		containerLayers[id] = c.blobs
		imageRefs[c.imageID]++
		for _, name := range c.volumeNames {
			volumeRefs[name]++
		}
	}
	volumes, err := e.listVolumes()
	e.mu.Unlock()
	if err != nil {
		return diskUsage{}, err
	}

	measured := map[string]map[string]uint64{"l": {}, "c": {}, "v": {}}
	guestMeasured := false
	if e.session != nil && e.session.Err() == nil {
		lines, err := e.session.Usage(ctx)
		guestMeasured = err == nil
		if err != nil && ctx.Err() == nil {
			log.Printf("measure guest disk usage: %v", err)
		}
		for _, line := range lines {
			fields := strings.Fields(line)
			if len(fields) != 3 || measured[fields[0]] == nil {
				continue
			}
			if size, err := strconv.ParseUint(fields[2], 10, 64); err == nil {
				measured[fields[0]][fields[1]] = size
			}
		}
	}

	result := diskUsage{
		Images: make([]map[string]any, 0, len(images)), Containers: containers,
		Volumes: make([]volumeUsage, 0, len(volumes)), BuildCache: e.buildCacheUsage(ctx),
	}
	layerBytes := measured["l"]
	layerSize := func(layer hostagent.Layer) uint64 {
		digest := strings.TrimPrefix(layer.Digest, "sha256:")
		if size, found := layerBytes[digest]; found {
			return size
		}
		var size uint64
		info, err := os.Stat(layer.Path)
		if err != nil && e.state != nil && validBlobName(digest) {
			// Persisted references may carry the path from a prior launch.
			info, err = os.Stat(filepath.Join(e.state.dir, "blobs", digest))
		}
		if err == nil {
			size = uint64(info.Size())
		}
		layerBytes[digest] = size
		return size
	}
	layerUsers := make(map[string]int)
	for _, image := range images {
		seen := make(map[string]bool)
		for _, l := range image.Layers {
			if !seen[l.Digest] {
				layerUsers[l.Digest]++
				seen[l.Digest] = true
			}
		}
	}
	for _, id := range slices.Sorted(maps.Keys(images)) {
		image := images[id]
		var size, shared uint64
		seen := make(map[string]bool)
		for _, l := range image.Layers {
			if seen[l.Digest] {
				continue
			}
			seen[l.Digest] = true
			used := layerSize(l)
			size += used
			if layerUsers[l.Digest] > 1 {
				shared += used
			}
		}
		result.Images = append(result.Images, map[string]any{
			"Id": id, "RepoTags": imageTags[id], "RepoDigests": imageDigests[id], "Created": image.Created, "Labels": image.Labels,
			"Size": size, "SharedSize": shared, "VirtualSize": size, "Containers": imageRefs[id],
		})
	}
	for _, row := range result.Containers {
		id := row["Id"].(string)
		rw := measured["c"][id]
		total := rw
		seen := make(map[string]bool)
		for _, l := range containerLayers[id] {
			if !seen[l.Digest] {
				total += layerSize(l)
				seen[l.Digest] = true
			}
		}
		row["SizeRw"], row["SizeRootFs"] = rw, total
	}
	for _, size := range layerBytes {
		result.LayersSize += size
	}
	for _, v := range volumes {
		row := volumeUsage{volume: v}
		row.UsageData.Size, row.UsageData.RefCount = -1, volumeRefs[v.Name]
		if e.remoteGuest {
			if guestMeasured {
				// VM volumes are materialized only when mounted.
				row.UsageData.Size = int64(measured["v"][v.Name])
			}
		} else if size, err := diskusage.Allocated(ctx, v.Mountpoint); err == nil {
			row.UsageData.Size = int64(size)
		}
		result.Volumes = append(result.Volumes, row)
	}
	return result, nil
}

type buildCacheUsage struct {
	ID, Parent, Type, Description string
	Parents                       []string
	InUse, Shared                 bool
	Size, UsageCount              int64
	CreatedAt                     time.Time
	LastUsedAt                    *time.Time
}

func (e *Engine) buildCacheUsage(ctx context.Context) []buildCacheUsage {
	out := []buildCacheUsage{}
	e.mu.Lock()
	c, err := e.find(e.defaultBuilder())
	running := err == nil && c.state == "running" && !c.paused && !c.removing
	e.mu.Unlock()
	if e.session == nil || !running {
		return out
	}
	probe, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var raw bytes.Buffer
	if err := e.runBuilderCommand(probe, c, nil, []string{"buildctl", "du", "--format", "{{json .}}"}, &raw, io.Discard); err != nil {
		return out
	}
	var records []struct {
		buildCacheUsage
		RecordType string
	}
	if err := json.NewDecoder(&raw).Decode(&records); err != nil {
		return out
	}
	for _, record := range records {
		row := record.buildCacheUsage
		row.Type = record.RecordType
		if len(row.Parents) > 0 {
			row.Parent = row.Parents[0]
		}
		out = append(out, row)
	}
	return out
}
