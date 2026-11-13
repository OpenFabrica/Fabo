//go:build linux || darwin

package localengine

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/libracoder/kiri/engine/internal/guestwire"
)

const (
	sigKill uint32 = 9
	sigTerm uint32 = 15
)

var linuxSignals = map[string]uint32{
	"HUP": 1, "INT": 2, "QUIT": 3, "ILL": 4, "TRAP": 5, "ABRT": 6, "IOT": 6, "BUS": 7,
	"FPE": 8, "KILL": 9, "USR1": 10, "SEGV": 11, "USR2": 12, "PIPE": 13, "ALRM": 14, "TERM": 15,
	"STKFLT": 16, "CHLD": 17, "CLD": 17, "CONT": 18, "STOP": 19, "TSTP": 20, "TTIN": 21, "TTOU": 22,
	"URG": 23, "XCPU": 24, "XFSZ": 25, "VTALRM": 26, "PROF": 27, "WINCH": 28, "IO": 29, "POLL": 29,
	"PWR": 30, "SYS": 31, "RTMIN": 34, "RTMAX": 64,
}

func parseSignal(raw string) (uint32, error) {
	if n, err := strconv.ParseUint(raw, 10, 32); err == nil && ((n >= 1 && n <= 31) || (n >= 34 && n <= 64)) {
		return uint32(n), nil
	}
	name := strings.TrimPrefix(strings.ToUpper(raw), "SIG")
	if signal, ok := linuxSignals[name]; ok {
		return signal, nil
	}
	if v, ok := strings.CutPrefix(name, "RTMIN+"); ok {
		if n, err := strconv.ParseUint(v, 10, 32); err == nil && n >= 1 && n <= 15 {
			return 34 + uint32(n), nil
		}
	}
	if v, ok := strings.CutPrefix(name, "RTMAX-"); ok {
		if n, err := strconv.ParseUint(v, 10, 32); err == nil && n >= 1 && n <= 14 {
			return 64 - uint32(n), nil
		}
	}
	return 0, fmt.Errorf("%w: invalid Linux signal %q", errInvalid, raw)
}

func (e *Engine) killHTTP(w http.ResponseWriter, r *http.Request) {
	signal := sigKill
	if raw := r.URL.Query().Get("signal"); raw != "" {
		var err error
		signal, err = parseSignal(raw)
		if err != nil {
			writeError(w, err)
			return
		}
	}
	c, err := e.lockOperation(r.PathValue("id"))
	if err == nil {
		defer c.operation.Unlock()
		var done <-chan struct{}
		done, _, _, err = e.signalTarget(c, false)
		if err == nil && (signal == sigKill || signal == sigTerm) {
			err = e.markManualStop(c)
		}
		if err == nil {
			err = e.signalRun(r.Context(), c, done, signal)
		}
	}
	if err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (e *Engine) stopHTTP(w http.ResponseWriter, r *http.Request) {
	e.stopOrRestartHTTP(w, r, false)
}

func (e *Engine) restartHTTP(w http.ResponseWriter, r *http.Request) {
	e.stopOrRestartHTTP(w, r, true)
}

func (e *Engine) stopOrRestartHTTP(w http.ResponseWriter, r *http.Request, restart bool) {
	c, err := e.lockOperation(r.PathValue("id"))
	if err != nil {
		writeError(w, err)
		return
	}
	defer c.operation.Unlock()
	if restart && c.run.Flags&guestwire.RunAutoRemove != 0 {
		writeError(w, fmt.Errorf("%w: cannot restart an auto-remove container", errConflict))
		return
	}
	done, signal, timeout, err := e.signalTarget(c, true)
	if err != nil {
		writeError(w, err)
		return
	}
	q := r.URL.Query()
	if raw := q.Get("signal"); raw != "" {
		signal, err = parseSignal(raw)
		if err != nil {
			writeError(w, err)
			return
		}
	}
	if raw := q.Get("t"); raw != "" {
		timeout, err = strconv.ParseInt(raw, 10, 64)
		if err != nil || !validTimeout(timeout) {
			writeError(w, fmt.Errorf("%w: invalid stop timeout %q", errInvalid, raw))
			return
		}
	}
	if err := e.markManualStop(c); err != nil {
		writeError(w, err)
		return
	}
	if done != nil {
		if err := e.stopWithContext(e.ctx, c, done, signal, timeout); err != nil {
			writeError(w, err)
			return
		}
	}
	if restart {
		if err := e.startContainer(c, false); err != nil {
			writeError(w, err)
			return
		}
		e.mu.Lock()
		e.emitContainer(c, "restart", nil)
		e.mu.Unlock()
	} else if done != nil {
		e.mu.Lock()
		e.emitContainer(c, "stop", nil)
		e.mu.Unlock()
	}
	w.WriteHeader(http.StatusNoContent)
}

// signalTarget snapshots the exit channel while the caller owns the
// container operation. Holding e.mu during a write would block event dispatch.
func (e *Engine) signalTarget(c *container, stopping bool) (<-chan struct{}, uint32, int64, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if c.state != "running" {
		if stopping && (c.state == "created" || c.state == "exited") {
			return nil, c.effectiveStopSignal(), c.stopTimeout, nil
		}
		return nil, 0, 0, fmt.Errorf("%w: container is not running", errConflict)
	}
	return c.attempt.done, c.effectiveStopSignal(), c.stopTimeout, nil
}

func (c *container) effectiveStopSignal() uint32 {
	if c.stopSignal == 0 {
		return sigTerm
	}
	return c.stopSignal
}

func validTimeout(seconds int64) bool {
	return seconds >= -1 && seconds <= int64(math.MaxInt64/time.Second)
}

func (e *Engine) signalRun(ctx context.Context, c *container, done <-chan struct{}, signal uint32) error {
	// The kernel can kill a frozen group, but a process must be thawed
	// before it can run a shutdown handler for a nonfatal signal.
	if signal != sigKill {
		if err := e.thawForSignal(ctx, c, done); err != nil {
			return err
		}
	}
	select {
	case <-done:
		return nil
	default:
	}
	if err := e.session.Signal(ctx, c.id, signal); err != nil {
		return err
	}
	e.mu.Lock()
	e.emitContainer(c, "kill", map[string]string{"signal": strconv.FormatUint(uint64(signal), 10)})
	e.mu.Unlock()
	return nil
}

func (e *Engine) stopWithContext(ctx context.Context, c *container, done <-chan struct{}, signal uint32, timeout int64) error {
	if err := e.signalRun(ctx, c, done, signal); err != nil {
		return err
	}
	if timeout < 0 || signal == sigKill {
		return waitStopped(ctx, done)
	}
	timer := time.NewTimer(time.Duration(timeout) * time.Second)
	defer timer.Stop()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		if err := e.signalRun(ctx, c, done, sigKill); err != nil {
			return err
		}
		return waitStopped(ctx, done)
	}
}

// StopContainers closes admission for container changes and gracefully stops
// the existing runs. The caller owns guest shutdown after this returns.
func (e *Engine) StopContainers(ctx context.Context) error {
	e.mu.Lock()
	e.closed = true
	type target struct {
		container *container
		attempt   *runAttempt
		pauseDone <-chan struct{}
		signal    uint32
		timeout   int64
	}
	targets := make([]target, 0, len(e.containers))
	for _, c := range e.containers {
		c.cancelRestart()
		if c.state == "starting" || c.state == "running" {
			targets = append(targets, target{c, c.attempt, c.attempt.pauseDone, c.effectiveStopSignal(), c.stopTimeout})
		}
	}
	e.mu.Unlock()

	results := make(chan error, len(targets))
	var stops sync.WaitGroup
	for _, c := range targets {
		stops.Go(func() {
			if c.pauseDone != nil {
				select {
				case <-c.pauseDone:
				case <-c.attempt.done:
					return
				case <-ctx.Done():
					results <- ctx.Err()
					return
				}
			}
			select {
			case <-c.attempt.done:
			case <-ctx.Done():
				results <- ctx.Err()
			case <-c.attempt.started:
				if err := e.stopWithContext(ctx, c.container, c.attempt.done, c.signal, c.timeout); err != nil {
					results <- fmt.Errorf("stop container %s: %w", c.container.id, err)
				}
			}
		})
	}
	stops.Wait()
	close(results)
	var err error
	for failure := range results {
		err = errors.Join(err, failure)
	}
	return err
}

func waitStopped(ctx context.Context, done <-chan struct{}) error {
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
