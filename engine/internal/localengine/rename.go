//go:build linux || darwin

package localengine

import (
	"fmt"
	"net/http"
	"regexp"
	"strings"
)

var containerNamePattern = regexp.MustCompile(`^[a-zA-Z0-9_.-]+$`)

func (e *Engine) renameHTTP(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimLeft(r.URL.Query().Get("name"), "/")
	if err := e.renameContainer(r.PathValue("id"), name); err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (e *Engine) renameContainer(key, name string) error {
	if !containerNamePattern.MatchString(name) {
		return fmt.Errorf("%w: invalid container name %q", errInvalid, name)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return fmt.Errorf("%w: daemon is shutting down", errConflict)
	}
	c, err := e.find(key)
	if err != nil {
		return err
	}
	if c.name == name {
		return fmt.Errorf("%w: container already has the name %q", errConflict, name)
	}
	if c.removing || e.managedBuilderName(name) != "" || e.managedBuilderName(c.name) != "" {
		return fmt.Errorf("%w: cannot rename a removing container or use a managed builder name", errConflict)
	}
	for _, other := range e.containers {
		if other.name == name {
			return fmt.Errorf("%w: container name %q already in use", errConflict, name)
		}
	}
	previous := c.name
	c.name = name
	if e.state != nil {
		if err := e.state.save(c); err != nil {
			c.name = previous
			return err
		}
	}
	e.emitContainer(c, "rename", map[string]string{"oldName": "/" + previous})
	return nil
}
