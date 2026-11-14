//go:build linux || darwin

package localengine

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"unicode"

	"github.com/OpenFabrica/FabriBay/engine/internal/guestwire"
)

func (e *Engine) setEnvironmentHTTP(w http.ResponseWriter, r *http.Request) {
	var input struct{ Env []string }
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&input); err != nil || input.Env == nil {
		writeError(w, fmt.Errorf("%w: env must be a list of NAME=value", errInvalid))
		return
	}
	for i, value := range input.Env {
		key, _, ok := strings.Cut(value, "=")
		if !ok || key == "" || strings.ContainsRune(value, 0) || strings.IndexFunc(key, unicode.IsSpace) >= 0 {
			writeError(w, fmt.Errorf("%w: env entry %d must be NAME=value", errInvalid, i+1))
			return
		}
	}
	key := r.PathValue("id")
	if err := e.setEnvironment(key, input.Env); err != nil {
		if errors.Is(err, errMissing) {
			err = fmt.Errorf("%w: No such container: %s", errMissing, key)
		}
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (e *Engine) setEnvironment(key string, env []string) error {
	c, err := e.lockOperation(key)
	if err != nil {
		return err
	}
	defer c.operation.Unlock()
	e.mu.Lock()
	if e.closed || c.removing || (c.state == "running" && c.run.Flags&guestwire.RunAutoRemove != 0) {
		e.mu.Unlock()
		return fmt.Errorf("%w: cannot change the environment in the container's current state", errConflict)
	}
	run := c.run
	run.Env = env
	if _, err := run.Frame(); err != nil {
		e.mu.Unlock()
		return fmt.Errorf("%w: environment exceeds guest protocol limits", errInvalid)
	}
	old, manual := c.run.Env, c.manualStopped
	var done <-chan struct{}
	signal, timeout := c.effectiveStopSignal(), c.stopTimeout
	if c.state == "running" {
		done = c.attempt.done
		c.manualStopped = true
	}
	c.run.Env = env
	if err := e.save(c); err != nil {
		c.run.Env, c.manualStopped = old, manual
		e.mu.Unlock()
		return err
	}
	e.emitContainer(c, "update", nil)
	e.mu.Unlock()

	if done == nil {
		return nil
	}
	if err := e.stopWithContext(e.ctx, c, done, signal, timeout); err != nil {
		return fmt.Errorf("environment saved for the next start: %w", err)
	}
	if err := e.startContainer(c, false); err != nil {
		return fmt.Errorf("environment saved but restart failed: %w", err)
	}
	return nil
}

func (e *Engine) setPortsHTTP(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Ports []struct {
			ContainerPort uint16  `json:"container_port"`
			HostPort      *uint16 `json:"host_port"`
			HostIP        string  `json:"host_ip"`
			Protocol      string  `json:"proto"`
		}
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&input); err != nil || input.Ports == nil {
		writeError(w, fmt.Errorf("%w: ports must be an array of port bindings", errInvalid))
		return
	}
	bindings := make([]portBinding, 0, len(input.Ports))
	for _, value := range input.Ports {
		protocol := value.Protocol
		if protocol == "" {
			protocol = "tcp"
		}
		if value.ContainerPort == 0 || value.HostPort == nil || (protocol != "tcp" && protocol != "udp") {
			writeError(w, fmt.Errorf("%w: expected container_port, host_port and TCP or UDP", errInvalid))
			return
		}
		host, err := parseBindHost(value.HostIP)
		if err != nil {
			writeError(w, err)
			return
		}
		bindings = append(bindings, portBinding{value.ContainerPort, protocol, host, *value.HostPort})
	}
	if err := e.setPorts(r.PathValue("id"), bindings); err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (e *Engine) setPorts(key string, bindings []portBinding) error {
	c, err := e.lockOperation(key)
	if err != nil {
		return err
	}
	defer c.operation.Unlock()
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed || c.removing {
		return fmt.Errorf("%w: cannot publish ports in the container's current state", errConflict)
	}
	if len(bindings) != 0 && (c.networkMode() == "none" || c.networkTarget() != "") {
		return fmt.Errorf("%w: port publishing requires a bridge or host network", errInvalid)
	}
	if len(bindings) != 0 && e.remoteGuest && e.guestRelay == nil {
		return fmt.Errorf("%w: this VM does not support published ports", errInvalid)
	}
	old := c.portBindings
	if slices.Equal(old, bindings) {
		return nil
	}

	var rollback []portBinding
	if c.state == "running" && c.attempt.ports != nil {
		rollback = c.attempt.ports.bound
		c.attempt.ports.close()
		c.attempt.ports = nil
	}
	if err := e.reserveUpdatedPorts(c, bindings); err != nil {
		return errors.Join(err, e.restoreUpdatedPorts(c, rollback))
	}
	c.portBindings = bindings
	if err := e.save(c); err != nil {
		c.portBindings = old
		return errors.Join(err, e.restoreUpdatedPorts(c, rollback))
	}
	if c.attempt != nil && c.attempt.ports != nil {
		c.attempt.ports.start()
	}
	e.emitContainer(c, "update", nil)
	return nil
}

// The caller owns both the container operation and e.mu while changing
// listeners. Container exit therefore cannot publish a stale binding.
func (e *Engine) reserveUpdatedPorts(c *container, bindings []portBinding) error {
	if c.state != "running" || len(bindings) == 0 {
		return nil
	}
	ports, err := e.reservePorts(bindings, c.networkAddress())
	if err == nil {
		c.attempt.ports = ports
	}
	return err
}

func (e *Engine) restoreUpdatedPorts(c *container, bindings []portBinding) error {
	if c.attempt != nil && c.attempt.ports != nil {
		c.attempt.ports.close()
		c.attempt.ports = nil
	}
	if err := e.reserveUpdatedPorts(c, bindings); err != nil {
		return fmt.Errorf("restore prior published ports: %w", err)
	}
	if c.attempt != nil && c.attempt.ports != nil {
		c.attempt.ports.start()
	}
	return nil
}
