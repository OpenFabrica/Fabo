//go:build linux || darwin

package localengine

import (
	"net/http"
	"strconv"

	"github.com/libracoder/kiri/engine/internal/guestwire"
)

func terminalSize(r *http.Request) [2]uint16 {
	q := r.URL.Query()
	rows, rowErr := strconv.ParseUint(q.Get("h"), 10, 16)
	cols, colErr := strconv.ParseUint(q.Get("w"), 10, 16)
	if rowErr != nil || colErr != nil {
		return [2]uint16{}
	}
	return [2]uint16{uint16(rows), uint16(cols)}
}

func (e *Engine) resizeHTTP(w http.ResponseWriter, r *http.Request) {
	size := terminalSize(r)
	if size[0] == 0 || size[1] == 0 {
		w.WriteHeader(http.StatusOK)
		return
	}
	c, err := e.lockOperation(r.PathValue("id"))
	if err != nil {
		writeError(w, err)
		return
	}
	defer c.operation.Unlock()
	e.mu.Lock()
	c.consoleSize = size
	running := c.state == "running" && c.run.Flags&guestwire.RunTTY != 0
	e.mu.Unlock()
	if running {
		if err := e.session.Resize(r.Context(), c.id, size[0], size[1]); err != nil {
			writeError(w, err)
			return
		}
	}
	w.WriteHeader(http.StatusOK)
}
