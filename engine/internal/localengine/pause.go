//go:build linux || darwin

package localengine

import (
	"context"
	"fmt"
	"net/http"
	"time"
)

func (e *Engine) pauseHTTP(w http.ResponseWriter, r *http.Request, paused bool) {
	c, err := e.lockOperation(r.PathValue("id"))
	if err == nil {
		defer c.operation.Unlock()
		err = e.setPaused(r.Context(), c, paused)
	}
	if err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// setPaused owns the container operation until the guest confirms the change.
func (e *Engine) setPaused(ctx context.Context, c *container, paused bool) error {
	e.mu.Lock()
	attempt := c.attempt
	if e.closed || c.state != "running" || attempt == nil {
		e.mu.Unlock()
		return fmt.Errorf("%w: container is not running", errConflict)
	}
	if c.paused == paused {
		e.mu.Unlock()
		if paused {
			return fmt.Errorf("%w: container is already paused", errConflict)
		}
		return fmt.Errorf("%w: container is not paused", errConflict)
	}
	changed := make(chan struct{})
	attempt.pauseDone = changed
	e.mu.Unlock()

	err := e.session.Freeze(ctx, c.id, paused)
	e.mu.Lock()
	defer e.mu.Unlock()
	attempt.pauseDone = nil
	close(changed)
	if c.attempt != attempt {
		return fmt.Errorf("%w: container exited during pause or resume", errConflict)
	}
	if err == nil {
		c.paused = paused
		if !paused && isBuildKit(c) {
			c.builderUsedAt = time.Now()
		}
		c.wakeHealth()
		action := "unpause"
		if paused {
			action = "pause"
		}
		e.emitContainer(c, action, nil)
	}
	return err
}

// thawForSignal targets the captured run. The run can exit while a guest
// acknowledgment is in flight, so a late reply cannot change its replacement.
func (e *Engine) thawForSignal(ctx context.Context, c *container, done <-chan struct{}) error {
	e.mu.Lock()
	attempt := c.attempt
	paused := attempt != nil && attempt.done == done && c.paused
	e.mu.Unlock()
	if !paused {
		return nil
	}
	err := e.session.Freeze(ctx, c.id, false)
	e.mu.Lock()
	defer e.mu.Unlock()
	if c.attempt != attempt {
		return nil
	}
	if err == nil {
		c.paused = false
		c.wakeHealth()
		e.emitContainer(c, "unpause", nil)
	}
	return err
}

func (c *container) dockerState() string {
	if c.restartTimer != nil {
		return "restarting"
	}
	if c.paused {
		return "paused"
	}
	return c.state
}
