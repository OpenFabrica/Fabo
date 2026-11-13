//go:build linux || darwin

package localengine

import (
	"encoding/json"
	"net/http"
	"os"
	"slices"

	v1 "github.com/google/go-containerregistry/pkg/v1"
)

type imageHistoryEntry struct {
	ID                 string `json:"Id"`
	Created            int64
	CreatedBy, Comment string
	Tags               []string
	Size               int64
}

func (e *Engine) imageHistoryHTTP(w http.ResponseWriter, ref string) {
	e.mu.Lock()
	image, err := e.findImage(ref)
	tags, _ := e.imageNames(image.ID)
	e.mu.Unlock()
	if err != nil {
		writeError(w, err)
		return
	}
	var metadata struct{ History []v1.History }
	if len(image.ConfigBlob) > 0 {
		if err := json.Unmarshal(image.ConfigBlob, &metadata); err != nil {
			writeError(w, err)
			return
		}
	}
	rows := make([]imageHistoryEntry, 0, len(metadata.History))
	layer := 0
	for _, h := range metadata.History {
		row := imageHistoryEntry{ID: "<missing>", Created: h.Created.Unix(), CreatedBy: h.CreatedBy, Comment: h.Comment}
		if h.Created.IsZero() {
			row.Created = 0
		}
		if !h.EmptyLayer && layer < len(image.Layers) {
			if info, err := os.Stat(image.Layers[layer].Path); err == nil {
				row.Size = info.Size()
			}
			layer++
		}
		rows = append(rows, row)
	}
	slices.Reverse(rows)
	if len(rows) > 0 {
		rows[0].ID, rows[0].Tags = image.ID, tags
	}
	writeJSON(w, http.StatusOK, rows)
}
