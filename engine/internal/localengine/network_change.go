//go:build linux || darwin

package localengine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"

	"github.com/OpenFabrica/FabriBay/engine/internal/guestwire"
)

func (e *Engine) connectNetworkHTTP(w http.ResponseWriter, r *http.Request) {
	e.changeNetworkHTTP(w, r, true)
}

func (e *Engine) disconnectNetworkHTTP(w http.ResponseWriter, r *http.Request) {
	e.changeNetworkHTTP(w, r, false)
}

func (e *Engine) changeNetworkHTTP(w http.ResponseWriter, r *http.Request, attach bool) {
	var body struct {
		Container      string
		EndpointConfig map[string]json.RawMessage
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil || body.Container == "" {
		writeError(w, fmt.Errorf("%w: expected a container ID", errInvalid))
		return
	}
	config, err := parseEndpoint(body.EndpointConfig)
	if err == nil {
		err = e.changeNetwork(r.Context(), body.Container, r.PathValue("id"), config, attach)
	}
	if err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// A single network operation owns allocation, guest acknowledgment and the
// metadata update. The container operation prevents a concurrent restart.
func (e *Engine) changeNetwork(ctx context.Context, containerID, networkID string, config endpointConfig, attach bool) error {
	c, err := e.lockOperation(containerID)
	if err != nil {
		return err
	}
	defer c.operation.Unlock()
	e.networkOps.Lock()
	defer e.networkOps.Unlock()

	e.mu.Lock()
	if e.closed || c.removing || c.state == "starting" || c.state == "removed" {
		e.mu.Unlock()
		return fmt.Errorf("%w: container network is unavailable", errConflict)
	}
	n, err := e.network(networkID)
	if err != nil {
		e.mu.Unlock()
		return err
	}
	if n.Bridge == "" {
		e.mu.Unlock()
		return fmt.Errorf("%w: network %q has no bridge", errInvalid, n.Name)
	}
	if c.networkMode() == "host" {
		e.mu.Unlock()
		return fmt.Errorf("%w: host-network containers cannot change bridge endpoints", errInvalid)
	}
	if c.networkTarget() != "" {
		e.mu.Unlock()
		return fmt.Errorf("%w: shared container networks cannot change bridge endpoints", errInvalid)
	}
	before := c.endpoints()
	running := c.state == "running"
	beforeLinks, nextLinks := c.links, c.links
	if attach && len(config.links) != 0 {
		if running {
			e.mu.Unlock()
			return fmt.Errorf("%w: attaching an endpoint with links requires a stopped container", errConflict)
		}
		links, err := e.resolveLinks(config.links, n.ID)
		if err != nil {
			e.mu.Unlock()
			return err
		}
		for _, link := range links {
			if e.linkWouldCycle(c.id, link.ID) {
				e.mu.Unlock()
				return fmt.Errorf("%w: linking %s to %s would create a startup dependency cycle", errConflict, c.name, link.Name)
			}
		}
		nextLinks = append(slices.Clone(beforeLinks), links...)
	} else if !attach && len(beforeLinks) != 0 {
		nextLinks = slices.DeleteFunc(slices.Clone(beforeLinks), func(link containerLink) bool { return link.NetworkID == n.ID })
	}
	next, changed, err := e.networkChange(c, n, before, config, attach, running)
	e.mu.Unlock()
	if err != nil {
		return err
	}

	apply := func(ctx context.Context, attach bool) error {
		if attach {
			return e.session.AttachNetwork(ctx, c.id, changed.Interface, e.liveNetwork(changed.Network))
		}
		return e.session.DetachNetwork(ctx, c.id, changed.Interface, changed.Network)
	}
	if running {
		if err := apply(ctx, attach); err != nil {
			return err
		}
	}
	e.mu.Lock()
	if c.state == "removed" {
		e.mu.Unlock()
		return fmt.Errorf("%w: container was removed", errMissing)
	}
	c.setEndpoints(next)
	c.links = nextLinks
	err = e.save(c)
	if err != nil {
		c.setEndpoints(before)
		c.links = beforeLinks
	} else {
		action := "disconnect"
		if attach {
			action = "connect"
		}
		e.emitNetwork(n, action, c.id)
	}
	undo := err != nil && running && c.state == "running"
	e.mu.Unlock()
	if undo {
		if undoErr := apply(e.ctx, !attach); undoErr != nil {
			return errors.Join(err, fmt.Errorf("restore guest endpoint: %w", undoErr))
		}
	}
	return err
}

// networkChange computes the new endpoint list without changing the
// container. The caller holds e.mu and networkOps.
func (e *Engine) networkChange(c *container, n dockerNetwork, current []containerEndpoint, config endpointConfig, attach, running bool) ([]containerEndpoint, containerEndpoint, error) {
	index := slices.IndexFunc(current, func(ep containerEndpoint) bool { return ep.ID == n.ID })
	if !attach {
		if index < 0 {
			return nil, containerEndpoint{}, fmt.Errorf("%w: container is not connected to %q", errMissing, n.Name)
		}
		if index == 0 && len(c.portBindings) != 0 && (running || len(current) == 1) {
			return nil, containerEndpoint{}, fmt.Errorf("%w: the primary endpoint is needed for published ports", errConflict)
		}
		return slices.Delete(slices.Clone(current), index, index+1), current[index], nil
	}
	if index >= 0 {
		return nil, containerEndpoint{}, fmt.Errorf("%w: container is already connected to %q", errConflict, n.Name)
	}
	if len(current) >= guestwire.MaxEndpoints {
		return nil, containerEndpoint{}, fmt.Errorf("%w: too many networks", errConflict)
	}
	var ifname string
	for i := range guestwire.MaxEndpoints {
		candidate := fmt.Sprintf("eth%d", i)
		if !slices.ContainsFunc(current, func(ep containerEndpoint) bool { return ep.Interface == candidate }) {
			ifname = candidate
			break
		}
	}
	if ifname == "" {
		return nil, containerEndpoint{}, fmt.Errorf("%w: no network interface is available", errConflict)
	}
	ep, err := e.newEndpoint(n, config, c.name, c.id, ifname)
	if err != nil {
		return nil, containerEndpoint{}, err
	}
	return append(slices.Clone(current), ep), ep, nil
}
