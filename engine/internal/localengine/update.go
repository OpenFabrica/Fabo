//go:build linux || darwin

package localengine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/OpenFabrica/Fabo/engine/internal/guestwire"
)

func (e *Engine) updateHTTP(w http.ResponseWriter, r *http.Request) {
	var fields map[string]json.RawMessage
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if err := json.NewDecoder(r.Body).Decode(&fields); err != nil || fields == nil {
		writeError(w, fmt.Errorf("%w: invalid resource update", errInvalid))
		return
	}
	c, err := e.lockOperation(r.PathValue("id"))
	if err == nil {
		defer c.operation.Unlock()
		err = e.updateContainer(r.Context(), c, fields)
	}
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"Warnings": []string{}})
}

// updateContainer runs under the container operation lock. The guest applies
// the live update before the new settings are published in the host record.
func (e *Engine) updateContainer(ctx context.Context, c *container, fields map[string]json.RawMessage) error {
	e.mu.Lock()
	before, attempt, running := c.run.Limits, c.attempt, c.state == "running"
	beforePolicy := c.restartPolicy
	e.mu.Unlock()
	policy, err := parseRestartPolicy(fields["RestartPolicy"])
	if err != nil {
		return err
	}
	delete(fields, "RestartPolicy")
	if policy.Name == "" {
		policy = beforePolicy
	}
	if c.run.Flags&guestwire.RunAutoRemove != 0 && policy.Name != "" && policy.Name != "no" {
		return fmt.Errorf("%w: auto-remove conflicts with restart policy", errInvalid)
	}
	after, err := mergeLimits(fields, before)
	if err != nil {
		return err
	}
	if before.ShmSize != after.ShmSize {
		return fmt.Errorf("%w: updating shared memory size is unsupported", errInvalid)
	}
	for field, raw := range fields {
		// Docker CLI versions can include default values for other resource
		// fields. Those zero values don't request a setting change.
		switch strings.TrimSpace(string(raw)) {
		case "0", "\"\"", "null", "false", "[]", "{}":
		default:
			return fmt.Errorf("%w: updating %s is unsupported", errInvalid, field)
		}
	}
	if before == after && beforePolicy == policy {
		return nil
	}
	if running && before != after {
		if err := e.session.UpdateLimits(ctx, c.id, after); err != nil {
			return err
		}
	}

	e.mu.Lock()
	if c.state == "removed" {
		e.mu.Unlock()
		return fmt.Errorf("%w: container was removed during update", errConflict)
	}
	c.run.Limits, c.restartPolicy = after, policy
	err = e.save(c)
	if err != nil {
		c.run.Limits, c.restartPolicy = before, beforePolicy
	} else if beforePolicy != policy && !c.canRestart(false) {
		c.cancelRestart()
	}
	if err == nil {
		e.emitContainer(c, "update", nil)
	}
	stillRunning := running && c.attempt == attempt && c.state == "running"
	e.mu.Unlock()
	if err != nil && stillRunning && before != after {
		if rollback := e.session.UpdateLimits(e.ctx, c.id, before); rollback != nil {
			err = errors.Join(err, fmt.Errorf("restore live limits: %w", rollback))
		}
	}
	return err
}
