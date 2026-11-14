//go:build linux || darwin

package localengine

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"slices"
	"time"

	"github.com/OpenFabrica/Fabo/engine/internal/guestwire"
)

const (
	healthStarting  = "starting"
	healthHealthy   = "healthy"
	healthUnhealthy = "unhealthy"
	healthLogSize   = 5
	healthOutputMax = 4096
)

type healthConfig struct {
	Test                                          []string      `json:",omitempty"`
	Interval, Timeout, StartPeriod, StartInterval time.Duration `json:",omitempty"`
	Retries                                       int           `json:",omitempty"`
}

func (image Image) healthMetadata() (*healthConfig, []string, error) {
	if len(image.ConfigBlob) == 0 {
		return nil, nil, nil
	}
	var metadata struct {
		Config struct {
			Healthcheck *healthConfig
			Shell       []string
		}
	}
	if err := json.Unmarshal(image.ConfigBlob, &metadata); err != nil {
		return nil, nil, fmt.Errorf("read image health config: %w", err)
	}
	return metadata.Config.Healthcheck, metadata.Config.Shell, nil
}

func mergeHealthConfig(override, inherited *healthConfig) (*healthConfig, error) {
	if override == nil && inherited == nil {
		return nil, nil
	}
	var check healthConfig
	if override != nil {
		check = *override
	}
	if inherited != nil {
		if len(check.Test) == 0 {
			check.Test = inherited.Test
		}
		for _, field := range []struct {
			value    *time.Duration
			fallback time.Duration
		}{
			{&check.Interval, inherited.Interval}, {&check.Timeout, inherited.Timeout},
			{&check.StartPeriod, inherited.StartPeriod}, {&check.StartInterval, inherited.StartInterval},
		} {
			if *field.value == 0 {
				*field.value = field.fallback
			}
		}
		if check.Retries == 0 {
			check.Retries = inherited.Retries
		}
	}
	for _, field := range []struct {
		name  string
		value time.Duration
	}{
		{"Interval", check.Interval}, {"Timeout", check.Timeout},
		{"StartPeriod", check.StartPeriod}, {"StartInterval", check.StartInterval},
	} {
		if field.value != 0 && field.value < time.Millisecond {
			return nil, fmt.Errorf("%w: %s in Healthcheck cannot be less than 1ms", errInvalid, field.name)
		}
	}
	if check.Retries < 0 {
		return nil, fmt.Errorf("%w: Retries in Healthcheck cannot be negative", errInvalid)
	}
	return &check, nil
}

func (check *healthConfig) command(shell []string) []string {
	if check == nil || len(check.Test) < 2 {
		return nil
	}
	switch check.Test[0] {
	case "CMD":
		return check.Test[1:]
	case "CMD-SHELL":
		if len(shell) == 0 {
			shell = []string{"/bin/sh", "-c"}
		}
		return append(slices.Clone(shell), check.Test[1:]...)
	default:
		return nil
	}
}

type healthResult struct {
	Start, End time.Time
	ExitCode   int32
	Output     string
}

type containerHealth struct {
	Status        string
	FailingStreak int
	Log           []healthResult
}

func (h *containerHealth) record(result healthResult, sinceStart time.Duration, check *healthConfig) {
	if len(h.Log) == healthLogSize {
		copy(h.Log, h.Log[1:])
		h.Log = h.Log[:healthLogSize-1]
	}
	h.Log = append(h.Log, result)
	switch {
	case result.ExitCode == 0:
		h.Status, h.FailingStreak = healthHealthy, 0
	case h.Status == healthStarting && sinceStart < check.StartPeriod:
		// While starting, failures in the grace period don't count.
	default:
		h.FailingStreak++
		if h.FailingStreak >= healthDefault(check.Retries, 3) {
			h.Status = healthUnhealthy
		}
	}
}

func healthDefault[T int | time.Duration](value, fallback T) T {
	if value == 0 {
		return fallback
	}
	return value
}

func (c *container) healthStatus() string {
	if c.health == nil {
		return "none"
	}
	return c.health.Status
}

// startHealth is called with e.mu held once the guest starts a new run.
func (e *Engine) startHealth(c *container) {
	argv := c.healthcheck.command(c.shell)
	if len(argv) == 0 {
		return
	}
	if c.health == nil {
		c.health = &containerHealth{Log: []healthResult{}}
	}
	c.health.Status, c.health.FailingStreak = healthStarting, 0
	attempt := c.attempt
	attempt.healthChanged = make(chan struct{})
	go e.monitorHealth(c, attempt, argv)
}

// wakeHealth is called under e.mu after a pause state change.
func (c *container) wakeHealth() {
	if c.attempt.healthChanged != nil {
		if c.paused {
			c.health.Status = healthUnhealthy
		}
		close(c.attempt.healthChanged)
		c.attempt.healthChanged = make(chan struct{})
	}
}

func (e *Engine) monitorHealth(c *container, attempt *runAttempt, argv []string) {
	check := c.healthcheck
	for {
		e.mu.Lock()
		if c.attempt != attempt || e.closed {
			e.mu.Unlock()
			return
		}
		paused, changed := c.paused, attempt.healthChanged
		interval := healthDefault(check.Interval, 30*time.Second)
		if c.health.Status == healthStarting && time.Since(attempt.startedAt) < check.StartPeriod {
			interval = healthDefault(check.StartInterval, 5*time.Second)
		}
		e.mu.Unlock()
		if paused {
			select {
			case <-changed:
				continue
			case <-attempt.done:
				return
			case <-e.ctx.Done():
				return
			}
		}
		timer := time.NewTimer(interval)
		select {
		case <-changed:
			timer.Stop()
			continue
		case <-attempt.done:
			timer.Stop()
			return
		case <-e.ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		result, skipped := e.probeHealth(c, attempt, changed, argv, healthDefault(check.Timeout, 30*time.Second))
		if skipped {
			continue
		}
		e.mu.Lock()
		if c.attempt == attempt && !e.closed && attempt.healthChanged == changed {
			before := c.health.Status
			c.health.record(result, result.Start.Sub(attempt.startedAt), check)
			if c.health.Status != before {
				e.emitContainer(c, "health_status: "+c.health.Status, nil)
				if c.run.Flags&guestwire.RunAutoRemove == 0 {
					if err := e.save(c); err != nil {
						log.Printf("save health status for %s: %v", c.id, err)
					}
				}
			}
		}
		e.mu.Unlock()
	}
}

func (e *Engine) probeHealth(c *container, attempt *runAttempt, changed <-chan struct{}, argv []string, timeout time.Duration) (result healthResult, skipped bool) {
	result = healthResult{Start: time.Now().UTC(), ExitCode: -1}
	defer func() { result.End = time.Now().UTC() }()
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		result.Output = err.Error()
		return
	}
	id := hex.EncodeToString(raw[:])
	e.mu.Lock()
	if c.attempt != attempt || e.closed || attempt.healthChanged != changed {
		e.mu.Unlock()
		skipped = true
		return
	}
	if e.execs == nil {
		e.execs = make(map[string]*execProcess)
	}
	e.execs[id] = &execProcess{
		spec:      guestwire.ExecSpec{ID: id, ContainerID: c.id, Argv: argv, Env: c.runtimeEnv(), Cwd: c.run.Cwd, User: c.run.User, Process: c.run.Process, Caps: c.run.Caps, Flags: c.run.Flags & guestwire.RunSecurityFlags},
		container: c, containerRun: attempt, healthProbe: changed,
	}
	e.mu.Unlock()
	defer func() {
		e.mu.Lock()
		delete(e.execs, id)
		e.mu.Unlock()
	}()
	x, attachment, err := e.startExec(e.ctx, id, false, [2]uint16{}, true)
	if err != nil {
		skipped = errors.Is(err, errHealthProbeBusy)
		result.Output = err.Error()
		return
	}
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	var output []byte
	for {
		select {
		case entry, ok := <-attachment.output:
			if !ok {
				// The exit handler closes the stream after publishing the exit code.
				e.mu.Lock()
				if x.exit != nil {
					result.ExitCode = *x.exit
				}
				e.mu.Unlock()
				result.Output = string(output[:min(len(output), healthOutputMax)])
				if len(output) > healthOutputMax {
					result.Output += "..."
				}
				return
			}
			if room := healthOutputMax + 1 - len(output); room > 0 {
				output = append(output, entry.data[:min(room, len(entry.data))]...)
				if len(output) > healthOutputMax {
					err = e.session.ExecOutputCredit(e.ctx, id, 0)
				} else {
					err = attachment.onOutput()
				}
				if err != nil {
					result.Output = err.Error()
					return
				}
			}
		case <-deadline.C:
			result.Output = fmt.Sprintf("Health check exceeded timeout (%s)", timeout)
			e.cancelInternalExec(attempt, x)
			return
		case <-changed:
			e.cancelInternalExec(attempt, x)
			return
		case <-attempt.done:
			return
		case <-e.ctx.Done():
			return
		}
	}
}

func (e *Engine) cancelInternalExec(run *runAttempt, x *execProcess) {
	// Unblock output pumps, then kill and reap the whole process group.
	select {
	case <-x.attempt.done:
		return
	default:
	}
	_ = e.session.ExecOutputCredit(e.ctx, x.spec.ID, 0)
	_ = e.session.Signal(e.ctx, x.spec.ID, sigKill)
	select {
	case <-x.attempt.done:
	case <-run.done:
	case <-e.ctx.Done():
	}
}
