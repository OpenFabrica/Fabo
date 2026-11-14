//go:build linux || darwin

package localengine

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"

	"github.com/OpenFabrica/FabriBay/engine/internal/guestwire"
)

func (e *Engine) removeHTTP(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	if err := e.remove(r.PathValue("id"), flagSet(query, "force"), flagSet(query, "v")); err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (e *Engine) remove(key string, force, volumes bool) error {
	c, err := e.lockOperation(key)
	if err != nil {
		return err
	}
	defer c.operation.Unlock()
	e.mu.Lock()
	if c.removing || c.state == "starting" || (c.state == "running" && !force) {
		e.mu.Unlock()
		return fmt.Errorf("%w: cannot remove a container in its current state", errConflict)
	}
	return e.removeLocked(e.ctx, c, volumes)
}

// removeLocked enters with e.mu and the container operation lock held. It
// releases e.mu while waiting for the guest, and keeps the operation reserved.
func (e *Engine) removeLocked(ctx context.Context, c *container, volumes bool) error {
	c.removing = true
	running := c.state == "running"
	var done <-chan struct{}
	if running {
		done = c.attempt.done
	}
	e.mu.Unlock()
	defer func() {
		e.mu.Lock()
		c.removing = false
		e.mu.Unlock()
	}()

	if running {
		if err := e.stopWithContext(ctx, c, done, sigKill, 0); err != nil {
			return err
		}
	}
	if c.run.Flags&guestwire.RunAutoRemove == 0 || !running {
		if err := e.session.Remove(ctx, c.id); err != nil {
			return err
		}
	}
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return errors.New("daemon is shutting down")
	}
	err := e.deleteRecord(c)
	e.mu.Unlock()
	if err != nil {
		return err
	}
	if volumes || c.run.Flags&guestwire.RunAutoRemove != 0 {
		err = e.removeAnonymousVolumes(ctx, c.anonymousVolumes)
	}
	e.mu.Lock()
	c.notifyRemoved()
	e.mu.Unlock()
	return err
}

// finishAutoRemoval is called under e.mu, sometimes by the session reader.
// Deleting guest volumes waits for an ACK, so it must run off that reader.
func (e *Engine) finishAutoRemoval(c *container) {
	if len(c.anonymousVolumes) == 0 || e.closed {
		c.notifyRemoved()
		return
	}
	e.volumeCleanups++
	go func() {
		if err := e.removeAnonymousVolumes(e.ctx, c.anonymousVolumes); err != nil {
			log.Printf("remove anonymous volumes for %s: %v", c.id, err)
		}
		e.mu.Lock()
		e.volumeCleanups--
		c.notifyRemoved()
		e.mu.Unlock()
	}()
}

// deleteRecord is called under e.mu. Existing waiters retain c after it is
// removed from the lookup map and can still read its exit status.
func (e *Engine) deleteRecord(c *container) error {
	if e.state != nil {
		err := os.Remove(filepath.Join(e.state.dir, c.id+".json"))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("delete container %s: %w", c.id, err)
		}
	}
	if c.attempt != nil {
		c.attempt.code, c.attempt.failure = c.exit, c.failure
		c.attempt.close()
		c.attempt = nil
	}
	c.closeOutputListeners()
	c.cancelRestart()
	c.state = "removed"
	delete(e.containers, c.id)
	e.emitContainer(c, "destroy", nil)
	for id, x := range e.execs {
		if x.container == c && !x.running {
			delete(e.execs, id)
		}
	}
	return nil
}

func (c *container) notifyRemoved() {
	if c.removed != nil {
		close(c.removed)
		c.removed = nil
	}
}
