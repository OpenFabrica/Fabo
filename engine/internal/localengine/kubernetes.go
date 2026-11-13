//go:build linux || darwin

package localengine

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"github.com/libracoder/kiri/engine/internal/guestwire"
)

const (
	kubernetesName       = "kiri-k8s"
	kubernetesLabel      = "dev.kiri.k8s"
	kubernetesAPI        = 6443
	kubernetesQueryLimit = 64 << 10
)

type kubernetesPort struct {
	port     uint16
	protocol string
}

func (e *Engine) startKubernetes(c *container) {
	if c.name == kubernetesName && c.labels[kubernetesLabel] == "1" {
		go e.watchKubernetes(c, c.attempt)
	}
}

func (e *Engine) watchKubernetes(c *container, attempt *runAttempt) {
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	routed := false
	for {
		select {
		case <-e.ctx.Done():
			return
		case <-attempt.done:
			return
		case <-ticker.C:
		}
		if output, ok := e.kubernetesServices(c, attempt); ok {
			if !routed {
				routed = e.session.KubernetesRoutes(e.ctx, c.id) == nil
			}
			e.syncKubernetesServices(c, attempt, parseKubernetesPorts(output))
		}
	}
}

func (e *Engine) kubernetesNode() *container {
	for _, c := range e.containers {
		if c.name == kubernetesName && c.labels[kubernetesLabel] == "1" && c.state == "running" {
			return c
		}
	}
	return nil
}

func (e *Engine) forwardKubernetesHTTP(w http.ResponseWriter, r *http.Request) {
	var body struct {
		IP   netip.Addr
		Port uint16
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil || !guestwire.KubernetesAddress(body.IP) || body.Port == 0 {
		writeError(w, fmt.Errorf("%w: not an address in the Kubernetes cluster", errInvalid))
		return
	}
	port, err := e.forwardKubernetes(r.Context(), body.IP, body.Port)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]uint16{"port": port})
}

func (e *Engine) forwardKubernetes(ctx context.Context, ip netip.Addr, port uint16) (uint16, error) {
	e.mu.Lock()
	node := e.kubernetesNode()
	if node == nil || e.closed {
		e.mu.Unlock()
		return 0, fmt.Errorf("%w: Kubernetes is not running", errConflict)
	}
	attempt := node.attempt
	e.mu.Unlock()
	if err := e.session.KubernetesRoutes(ctx, node.id); err != nil {
		return 0, fmt.Errorf("route to Kubernetes: %w", err)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed || node.state != "running" || node.attempt != attempt {
		return 0, fmt.Errorf("%w: Kubernetes node has stopped", errConflict)
	}
	forward, err := e.reservePorts([]portBinding{{HostIP: "127.0.0.1", ContainerPort: port, Protocol: "tcp"}}, ip)
	if err != nil {
		return 0, err
	}
	if attempt.kubernetesShares == nil {
		attempt.kubernetesShares = make(map[uint16]*publishedPorts)
	}
	bound := forward.bound[0].HostPort
	attempt.kubernetesShares[bound] = forward
	forward.start()
	return bound, nil
}

func (e *Engine) unforwardKubernetesHTTP(w http.ResponseWriter, r *http.Request) {
	port, err := strconv.ParseUint(r.PathValue("port"), 10, 16)
	if err != nil || port == 0 {
		writeError(w, fmt.Errorf("%w: invalid Kubernetes forward port", errInvalid))
		return
	}
	e.mu.Lock()
	if node := e.kubernetesNode(); node != nil {
		shares := node.attempt.kubernetesShares
		if forward := shares[uint16(port)]; forward != nil {
			forward.close()
			delete(shares, uint16(port))
		}
	}
	e.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func parseKubernetesPorts(output string) map[kubernetesPort]bool {
	ports := make(map[kubernetesPort]bool)
	for _, field := range strings.Fields(output) {
		number, protocol, _ := strings.Cut(field, "/")
		port, err := strconv.ParseUint(number, 10, 16)
		protocol = strings.ToLower(protocol)
		if err == nil && port != 0 && (protocol == "tcp" || protocol == "udp") {
			ports[kubernetesPort{uint16(port), protocol}] = true
		}
	}
	return ports
}

func (e *Engine) syncKubernetesServices(c *container, attempt *runAttempt, wanted map[kubernetesPort]bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed || c.state != "running" || c.attempt != attempt {
		return
	}
	for port, forward := range attempt.kubernetesPorts {
		if !wanted[port] {
			forward.close()
			delete(attempt.kubernetesPorts, port)
		}
	}
	address := c.networkAddress()
	if !address.IsValid() {
		return
	}
	hostIP := "127.0.0.1"
	for _, b := range c.portBindings {
		if b.ContainerPort == kubernetesAPI && b.Protocol == "tcp" && (b.HostIP == "0.0.0.0" || b.HostIP == "") {
			hostIP = "0.0.0.0"
			break
		}
	}
	for port := range wanted {
		if attempt.kubernetesPorts[port] != nil {
			continue
		}
		binding := portBinding{HostIP: hostIP, HostPort: port.port, ContainerPort: port.port, Protocol: port.protocol}
		forward, err := e.reservePorts([]portBinding{binding}, address)
		if err != nil {
			// An occupied host port can become available on a later poll.
			continue
		}
		if attempt.kubernetesPorts == nil {
			attempt.kubernetesPorts = make(map[kubernetesPort]*publishedPorts)
		}
		attempt.kubernetesPorts[port] = forward
		forward.start()
	}
}

func (e *Engine) kubernetesServices(c *container, attempt *runAttempt) (string, bool) {
	const query = `jsonpath={range .items[?(@.spec.type=="LoadBalancer")]}{range .spec.ports[*]}{.port}/{.protocol} {end}{end}`
	id, err := newExecID()
	if err != nil {
		return "", false
	}
	e.mu.Lock()
	if e.closed || c.state != "running" || c.paused || c.attempt != attempt {
		e.mu.Unlock()
		return "", false
	}
	x := &execProcess{container: c, containerRun: attempt, internal: true, stdout: true}
	x.spec = guestwire.ExecSpec{ID: id, ContainerID: c.id, Argv: []string{"kubectl", "get", "svc", "-A", "-o", query}, Env: c.runtimeEnv(), Cwd: c.run.Cwd, User: c.run.User, Process: c.run.Process, Caps: c.run.Caps, Flags: c.run.Flags & guestwire.RunSecurityFlags}
	if e.execs == nil {
		e.execs = make(map[string]*execProcess)
	}
	e.execs[id] = x
	e.mu.Unlock()
	defer func() {
		e.mu.Lock()
		delete(e.execs, id)
		e.mu.Unlock()
	}()
	ctx, cancel := context.WithTimeout(e.ctx, 20*time.Second)
	defer cancel()
	_, attachment, err := e.startExec(ctx, id, false, [2]uint16{}, true)
	if err != nil {
		return "", false
	}
	var output []byte
	for {
		select {
		case record, ok := <-attachment.output:
			if !ok {
				e.mu.Lock()
				success := x.exit != nil && *x.exit == 0
				e.mu.Unlock()
				return string(output), success
			}
			if record.stream == 1 {
				output = append(output, record.data...)
			}
			if len(output) > kubernetesQueryLimit {
				e.cancelInternalExec(attempt, x)
				return "", false
			}
			if err := attachment.onOutput(); err != nil {
				return "", false
			}
		case <-ctx.Done():
			e.cancelInternalExec(attempt, x)
			return "", false
		case <-attempt.done:
			return "", false
		}
	}
}
