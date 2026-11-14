//go:build linux || darwin

package localengine

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/OpenFabrica/Fabo/engine/internal/guestwire"
)

// readyBuilder starts and probes the managed builder. When create is false,
// an absent builder leaves no cache to operate on.
func (e *Engine) readyBuilder(ctx context.Context, create bool) (*container, error) {
	e.builderOps.Lock()
	defer e.builderOps.Unlock()

	e.mu.Lock()
	c, err := e.find(e.defaultBuilder())
	e.mu.Unlock()
	if err != nil {
		if !create {
			return nil, err
		}
		id, err := e.newManagedBuilder(ctx, e.defaultBuilder())
		if err != nil {
			return nil, err
		}
		e.mu.Lock()
		c = e.containers[id]
		e.mu.Unlock()
		if c == nil {
			return nil, fmt.Errorf("%w: the managed builder was removed", errMissing)
		}
	}

	c.operation.Lock()
	e.mu.Lock()
	running, paused := c.state == "running", c.paused
	e.mu.Unlock()
	if paused {
		err = e.setPaused(ctx, c, false)
	} else if !running {
		err = e.startContainer(c, false)
	} else {
		err = nil
	}
	c.operation.Unlock()
	if err != nil {
		return nil, err
	}

	ready, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	for {
		err = e.runBuilderCommand(ready, c, nil, []string{"buildctl", "debug", "workers"}, io.Discard, io.Discard)
		if err == nil {
			return c, nil
		}
		e.mu.Lock()
		running := c.state == "running"
		e.mu.Unlock()
		if !running || ready.Err() != nil {
			return nil, fmt.Errorf("Fabo BuildKit is not ready: %w", err)
		}
		select {
		case <-ready.Done():
			return nil, ready.Err()
		case <-time.After(150 * time.Millisecond):
		}
	}
}

func (e *Engine) runBuilderCommand(ctx context.Context, c *container, env, argv []string, stdout, stderr io.Writer) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	id, err := newExecID()
	if err != nil {
		return err
	}
	e.mu.Lock()
	if e.execs == nil {
		e.execs = make(map[string]*execProcess)
	}
	e.execs[id] = &execProcess{
		spec:      guestwire.ExecSpec{ID: id, ContainerID: c.id, Argv: argv, Env: mergeEnv(c.run.Env, env), Cwd: "/", Process: c.run.Process, Caps: c.run.Caps, Flags: c.run.Flags & guestwire.RunSecurityFlags},
		container: c, containerRun: c.attempt, internal: true,
	}
	e.mu.Unlock()
	defer func() {
		e.mu.Lock()
		delete(e.execs, id)
		e.mu.Unlock()
	}()

	x, output, err := e.startExec(ctx, id, false, [2]uint16{}, true)
	if err != nil {
		return err
	}
	for {
		select {
		case <-ctx.Done():
			e.stopBuilderCommand(x)
			return ctx.Err()
		case entry, ok := <-output.output:
			if !ok {
				e.mu.Lock()
				code := x.exit
				e.mu.Unlock()
				if code == nil {
					e.stopBuilderCommand(x)
					return fmt.Errorf("build command output was interrupted")
				}
				if *code != 0 {
					return fmt.Errorf("%s exited with code %d", argv[0], *code)
				}
				return ctx.Err()
			}
			writer := stdout
			if entry.stream == 2 {
				writer = stderr
			}
			if _, err := writer.Write(entry.data); err != nil {
				e.stopBuilderCommand(x)
				return err
			}
			if err := output.onOutput(); err != nil {
				e.stopBuilderCommand(x)
				return err
			}
		}
	}
}

func (e *Engine) stopBuilderCommand(x *execProcess) {
	select {
	case <-x.attempt.done:
		return
	default:
	}
	cleanup, cancel := context.WithTimeout(e.ctx, 5*time.Second)
	defer cancel()
	// Release blocked output writers before interrupting buildctl.
	_ = e.session.ExecOutputCredit(cleanup, x.spec.ID, 0)
	_ = e.session.Signal(cleanup, x.spec.ID, 2)
	select {
	case <-x.attempt.done:
		return
	case <-cleanup.Done():
		return
	case <-time.After(2 * time.Second):
		_ = e.session.Signal(cleanup, x.spec.ID, sigKill)
	}
	select {
	case <-x.attempt.done:
	case <-cleanup.Done():
	}
}
