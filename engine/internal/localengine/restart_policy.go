//go:build linux || darwin

package localengine

import (
	"encoding/json"
	"fmt"
	"log"
	"slices"
	"time"

	"github.com/libracoder/kiri/engine/internal/guestwire"
)

type restartPolicy struct {
	Name              string
	MaximumRetryCount int
}

func parseRestartPolicy(raw json.RawMessage) (restartPolicy, error) {
	var p restartPolicy
	if len(raw) != 0 {
		if err := json.Unmarshal(raw, &p); err != nil {
			return p, fmt.Errorf("%w: invalid restart policy", errInvalid)
		}
	}
	switch p.Name {
	case "", "no", "always", "unless-stopped", "on-failure":
	default:
		return p, fmt.Errorf("%w: unknown restart policy %q", errInvalid, p.Name)
	}
	if p.MaximumRetryCount < 0 || p.MaximumRetryCount > 0 && p.Name != "on-failure" {
		return p, fmt.Errorf("%w: maximum retries require the on-failure policy", errInvalid)
	}
	return p, nil
}

func (p restartPolicy) inspect() restartPolicy {
	if p.Name == "" {
		p.Name = "no"
	}
	return p
}

func (c *container) canRestart(boot bool) bool {
	if c.state != "exited" || c.removing || c.run.Flags&guestwire.RunAutoRemove != 0 {
		return false
	}
	switch c.restartPolicy.Name {
	case "always":
		return boot || !c.manualStopped
	case "unless-stopped":
		return !c.manualStopped
	case "on-failure":
		maxRetries := c.restartPolicy.MaximumRetryCount
		return !boot && !c.manualStopped && c.exit != 0 && (maxRetries == 0 || c.restartCount < maxRetries)
	default:
		return false
	}
}

// queueRestart and cancelRestart run under e.mu. The timer checks its
// identity after taking the same operation lock as explicit lifecycle calls.
func (e *Engine) queueRestart(c *container, boot bool, delay time.Duration) {
	var timer *time.Timer
	timer = time.AfterFunc(delay, func() {
		c.operation.Lock()
		defer c.operation.Unlock()
		e.mu.Lock()
		if c.restartTimer != timer {
			e.mu.Unlock()
			return
		}
		allowed := !e.closed && e.ctx.Err() == nil && e.session.Err() == nil && c.canRestart(boot)
		if !allowed {
			c.restartTimer = nil
			e.mu.Unlock()
			return
		}
		if boot {
			starting := func(id string) bool {
				owner := e.containers[id]
				return owner != nil && (owner.state == "starting" || owner.restartTimer != nil)
			}
			if starting(c.networkTarget()) || slices.ContainsFunc(c.links, func(link containerLink) bool { return starting(link.ID) }) {
				e.queueRestart(c, true, 100*time.Millisecond)
				e.mu.Unlock()
				return
			}
		}
		e.mu.Unlock()
		if err := e.startContainer(c, true); err != nil {
			e.mu.Lock()
			if c.restartTimer == timer {
				c.restartTimer = nil
			}
			e.mu.Unlock()
			log.Printf("restart container %s: %v", c.id, err)
		}
	})
	c.restartTimer = timer
}

func (c *container) cancelRestart() {
	if c.restartTimer != nil {
		c.restartTimer.Stop()
		c.restartTimer = nil
	}
}

func (e *Engine) markManualStop(c *container) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if c.manualStopped || c.state == "created" {
		c.cancelRestart()
		return nil
	}
	c.manualStopped = true
	if err := e.save(c); err != nil {
		c.manualStopped = false
		return err
	}
	c.cancelRestart()
	return nil
}
