//go:build linux || darwin

package localengine

import (
	"context"
	"errors"
	"strings"
	"time"
)

func isBuildKit(c *container) bool {
	return strings.HasPrefix(c.name, "buildx_buildkit_")
}

// holdBuilder covers the gaps between BuildKit commands during a classic
// build or prune, such as an archive upload while no exec is running.
func (e *Engine) holdBuilder() func() {
	e.mu.Lock()
	e.builderUsers++
	e.mu.Unlock()
	return func() {
		e.mu.Lock()
		e.builderUsers--
		if c, err := e.find(e.defaultBuilder()); err == nil && c.state == "running" {
			c.builderUsedAt = time.Now()
		}
		e.mu.Unlock()
	}
}

// PauseIdleBuilders freezes the managed builder even on hosts without VM
// sleep. If a VM is also idle, other Buildx builders may be parked when
// they are the only running workloads.
func (e *Engine) PauseIdleBuilders(ctx context.Context, now time.Time, vmIdle bool) error {
	e.mu.Lock()
	if e.closed || e.builderUsers != 0 {
		e.mu.Unlock()
		return nil
	}
	if vmIdle {
		for _, c := range e.containers {
			if c.state == "running" && !isBuildKit(c) {
				vmIdle = false
				break
			}
		}
	}
	var candidates []*container
	for _, c := range e.containers {
		if c.state == "running" && !c.paused && (e.managedBuilderName(c.name) != "" || vmIdle && isBuildKit(c)) {
			candidates = append(candidates, c)
		}
	}
	e.mu.Unlock()
	if len(candidates) == 0 {
		return nil
	}
	settings, err := e.state.loadBuilderSettings()
	if err != nil {
		return err
	}
	idle := time.Duration(builderIdleSeconds(settings)) * time.Second
	if idle == 0 {
		return nil
	}
	var pauseErr error
	for _, c := range candidates {
		pauseErr = errors.Join(pauseErr, e.pauseIdleBuilder(ctx, c, now.Add(-idle)))
	}
	return pauseErr
}

func (e *Engine) pauseIdleBuilder(ctx context.Context, c *container, cutoff time.Time) error {
	if !c.operation.TryLock() {
		return nil
	}
	defer c.operation.Unlock()
	e.mu.Lock()
	idle := !e.closed && e.builderUsers == 0 && c.state == "running" && !c.paused && !c.builderUsedAt.After(cutoff)
	if idle {
		for _, x := range e.execs {
			if x.container == c && x.running {
				idle = false
				break
			}
		}
	}
	e.mu.Unlock()
	if !idle {
		return nil
	}
	return e.setPaused(ctx, c, true)
}
