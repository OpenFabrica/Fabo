//go:build linux || darwin

package localengine

import (
	"fmt"
	"net/http"
	"strings"
)

func (e *Engine) topHTTP(w http.ResponseWriter, r *http.Request) {
	e.mu.Lock()
	c, err := e.find(r.PathValue("id"))
	var run *runAttempt
	if err == nil {
		if c.state != "running" || c.attempt == nil {
			err = fmt.Errorf("%w: container is not running", errConflict)
		} else {
			run = c.attempt
		}
	}
	e.mu.Unlock()
	if err != nil {
		writeError(w, err)
		return
	}
	rows, err := e.session.Top(r.Context(), c.id)
	select {
	case <-run.done:
		err = fmt.Errorf("%w: container run ended while listing processes", errConflict)
	default:
	}
	if err != nil {
		writeError(w, err)
		return
	}
	titles := []string{"UID", "PID", "PPID", "C", "STIME", "TTY", "TIME", "CMD"}
	processes := make([][]string, 0, len(rows))
	for _, row := range rows {
		columns := strings.SplitN(row, "\t", len(titles))
		if len(columns) != len(titles) {
			writeError(w, fmt.Errorf("invalid guest process row"))
			return
		}
		processes = append(processes, columns)
	}
	writeJSON(w, http.StatusOK, map[string]any{"Titles": titles, "Processes": processes})
}
