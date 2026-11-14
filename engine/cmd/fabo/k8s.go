//go:build linux || darwin

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"github.com/OpenFabrica/FabriBay/engine/internal/guestwire"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const k8sName = "fabo-k8s"
const k8sImage = "rancher/k3s:v1.37.1-k3s1"
const k8sPortLabel = "dev.fabo.k8s.port"
const k8sConfigLabel = "dev.fabo.k8s.config"
const k8sUsage = "usage: fabo k8s start [--use] | stop | status | delete | set [--lan on|off] [--tls-san NAME,…] [--port N]"

type k8sConfig struct {
	LAN  bool     `json:"lan"`
	SAN  []string `json:"tls_san"`
	Port int      `json:"port,omitempty"`
}

func (c k8sConfig) port() int {
	if c.Port == 0 {
		return 6443
	}
	return c.Port
}

type k8sNode struct {
	State struct {
		Running bool
		Status  string
	}
	Config          struct{ Labels map[string]string }
	NetworkSettings struct {
		Ports map[string][]struct{ HostPort string }
	}
}

func (n k8sNode) port() int {
	if ports := n.NetworkSettings.Ports["6443/tcp"]; len(ports) > 0 {
		if port, e := strconv.Atoi(ports[0].HostPort); e == nil && port > 0 {
			return port
		}
	}
	port, _ := strconv.Atoi(n.Config.Labels[k8sPortLabel])
	if port == 0 {
		return 6443
	}
	return port
}
func validK8sSAN(s string) bool {
	if net.ParseIP(s) != nil {
		return true
	}
	if s == "" || len(s) > 253 {
		return false
	}
	for _, label := range strings.Split(strings.TrimPrefix(s, "*."), ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, c := range []byte(label) {
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-') {
				return false
			}
		}
	}
	return true
}
func k8sSettings(before k8sConfig, args []string) (k8sConfig, error) {
	c := before
	for len(args) > 0 {
		if len(args) < 2 {
			return c, errors.New(k8sUsage)
		}
		switch args[0] {
		case "--lan":
			switch args[1] {
			case "on", "true", "1":
				c.LAN = true
			case "off", "false", "0":
				c.LAN = false
			default:
				return c, errors.New("--lan requires on or off")
			}
		case "--port":
			n, e := strconv.Atoi(args[1])
			if e != nil || n < 1024 || n > 65535 {
				return c, errors.New("the API server port must be from 1024 to 65535")
			}
			c.Port = n
			if n == 6443 {
				c.Port = 0
			}
		case "--tls-san":
			c.SAN = []string{}
			for _, s := range strings.Split(args[1], ",") {
				s = strings.TrimSpace(s)
				if s == "" {
					continue
				}
				if !validK8sSAN(s) {
					return c, fmt.Errorf("not an IP address or domain name: %s", s)
				}
				c.SAN = append(c.SAN, s)
			}
		default:
			return c, errors.New(k8sUsage)
		}
		args = args[2:]
	}
	return c, nil
}
func k8sFolders() []string {
	dirs := []string{"/home", "/mnt", "/media"}
	if runtime.GOOS == "darwin" {
		dirs = []string{"/Users", "/Volumes"}
	}
	out := []string{}
	for _, p := range dirs {
		if s, e := os.Stat(p); e == nil && s.IsDir() {
			out = append(out, p)
		}
	}
	return out
}
func k8sMade(c k8sConfig) string {
	san := c.SAN
	if san == nil {
		san = []string{}
	}
	raw, _ := json.Marshal([]any{c.LAN, san, c.port(), k8sFolders()})
	return string(raw)
}
func k8sPortFree(port int, lan bool) bool {
	conn, e := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 300*time.Millisecond)
	if e == nil {
		conn.Close()
		return false
	}
	host := "127.0.0.1"
	if lan {
		host = "0.0.0.0"
	}
	l, e := net.Listen("tcp4", fmt.Sprintf("%s:%d", host, port))
	if e != nil {
		return false
	}
	l.Close()
	return true
}
func k8sPickPort(want int, lan bool) (int, error) {
	for p := want; p <= min(want+20, 65535); p++ {
		if k8sPortFree(p, lan) {
			return p, nil
		}
	}
	return 0, fmt.Errorf("ports %d–%d are all in use; choose another with fabo k8s set --port N", want, min(want+20, 65535))
}
func k8sSpec(c k8sConfig, port int) map[string]any {
	cmd := []string{"server", "--disable", "traefik", "--tls-san", "127.0.0.1", "--tls-san", "localhost", "--write-kubeconfig-mode", "644"}
	for _, s := range c.SAN {
		cmd = append(cmd, "--tls-san", s)
	}
	binds := []string{k8sName + ":/var/lib/rancher/k3s"}
	for _, p := range k8sFolders() {
		binds = append(binds, p+":"+p)
	}
	host := "127.0.0.1"
	if c.LAN {
		host = "0.0.0.0"
	}
	return map[string]any{"Image": k8sImage, "Cmd": cmd, "Hostname": "fabo", "Labels": map[string]string{"dev.fabo.k8s": "1", k8sPortLabel: strconv.Itoa(port), k8sConfigLabel: k8sMade(c)}, "ExposedPorts": map[string]any{"6443/tcp": map[string]any{}}, "HostConfig": map[string]any{"Privileged": true, "Tmpfs": map[string]string{"/run": "", "/var/run": ""}, "Binds": binds, "PortBindings": map[string]any{"6443/tcp": []map[string]string{{"HostIp": host, "HostPort": strconv.Itoa(port)}}}, "RestartPolicy": map[string]string{"Name": "unless-stopped"}}}
}
func (c *commandClient) k8sNode() (*k8sNode, error) {
	var node k8sNode
	err := c.request("GET", "/containers/"+k8sName+"/json", nil, &node)
	var api *commandError
	if errors.As(err, &api) && api.status == 404 {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &node, nil
}
func (i installation) k8sConfig() (k8sConfig, error) {
	c := k8sConfig{SAN: []string{}}
	raw, e := os.ReadFile(filepath.Join(i.root(), "k8s.json"))
	if errors.Is(e, os.ErrNotExist) {
		return c, nil
	}
	if e != nil {
		return c, e
	}
	e = json.Unmarshal(raw, &c)
	if e == nil {
		if c.port() < 1024 || c.port() > 65535 {
			e = errors.New("invalid stored Kubernetes port")
		}
		for _, s := range c.SAN {
			if !validK8sSAN(s) {
				e = fmt.Errorf("invalid stored certificate name %q", s)
			}
		}
	}
	return c, e
}
func k8sCommand(args []string) error {
	if len(args) == 0 {
		return errors.New(k8sUsage)
	}
	verb := args[0]
	use := len(args) == 2 && args[1] == "--use"
	switch verb {
	case "start":
		if len(args) != 1 && !use {
			return errors.New(k8sUsage)
		}
	case "stop", "status", "delete":
		if len(args) != 1 {
			return errors.New(k8sUsage)
		}
	case "set":
		if _, e := k8sSettings(k8sConfig{}, args[1:]); e != nil {
			return e
		}
	default:
		return errors.New(k8sUsage)
	}
	in, e := locate()
	if e != nil {
		return e
	}
	c, e := in.runningClient()
	if e != nil {
		return e
	}
	defer c.http.CloseIdleConnections()
	cfg, e := in.k8sConfig()
	if e != nil {
		return e
	}
	switch verb {
	case "start":
		return in.k8sStart(c, cfg, use)
	case "stop":
		if e := machineAction(c, k8sName, "stop"); e != nil {
			return e
		}
		fmt.Println("Kubernetes stopped (its state is kept; `fabo k8s start` resumes it)")
	case "delete":
		for _, path := range []string{"/containers/" + k8sName + "?force=1", "/volumes/" + k8sName} {
			if e := c.request("DELETE", path, nil, nil); e != nil {
				var api *commandError
				if !errors.As(e, &api) || api.status != 404 {
					return e
				}
			}
		}
		if _, e := updateKubeconfig(in.kubePath(), "", in.names.context, false, true); e != nil {
			return e
		}
		if e := os.Remove(filepath.Join(in.root(), "kubeconfig")); e != nil && !errors.Is(e, os.ErrNotExist) {
			return e
		}
		fmt.Println("Kubernetes deleted, with its state")
	case "status":
		node, e := c.k8sNode()
		if e != nil {
			return e
		}
		if node == nil {
			fmt.Println("Kubernetes: not created (fabo k8s start)")
			return nil
		}
		fmt.Printf("Kubernetes: %s, API on 127.0.0.1:%d\n", node.State.Status, node.port())
		if node.State.Running {
			if e := in.k8sSync(c, false, node.port()); e != nil {
				fmt.Fprintf(os.Stderr, "kubectl context not written: %v\n", e)
			}
			code, out, e := c.exec(k8sName, []string{"timeout", "-s", "KILL", "20", "kubectl", "get", "nodes", "-o", "wide"})
			if e != nil {
				return e
			}
			fmt.Print(out)
			if code != 0 {
				return fmt.Errorf("kubectl exited %d", code)
			}
		}
	case "set":
		next, e := k8sSettings(cfg, args[1:])
		if e != nil {
			return e
		}
		node, e := c.k8sNode()
		if e != nil {
			return e
		}
		if next.port() != cfg.port() && (node == nil || node.port() != next.port()) && !k8sPortFree(next.port(), next.LAN) {
			return fmt.Errorf("port %d is in use; choose another", next.port())
		}
		raw, e := json.Marshal(next)
		if e != nil {
			return e
		}
		if e = writePrivateFile(filepath.Join(in.root(), "k8s.json"), raw); e != nil {
			return e
		}
		fmt.Printf("API server port %d; LAN %t; extra certificate names: %s\n", next.port(), next.LAN, strings.Join(next.SAN, ", "))
		if node == nil || reflect.DeepEqual(cfg, next) {
			return nil
		}
		if !node.State.Running {
			fmt.Println("they apply when the cluster next starts")
			return nil
		}
		fmt.Println("recreating the node with them (the cluster's state is kept)…")
		if e = c.request("DELETE", "/containers/"+k8sName+"?force=1", nil, nil); e != nil {
			return e
		}
		return in.k8sStart(c, next, false)
	}
	return nil
}
func (i installation) runningClient() (*commandClient, error) {
	probe := socketClient(i.socket())
	defer probe.CloseIdleConnections()
	if !alive(probe) {
		return nil, errors.New("fabo daemon is not running: open Fabo.app or run `fabo daemon`")
	}
	return newCommandClient(i.socket()), nil
}
func (c *commandClient) pullImage(image string) error {
	_, query := machineImage(image)
	var raw []byte
	if e := c.request("POST", "/images/create?"+query.Encode(), nil, &raw); e != nil {
		return e
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	for dec.More() {
		var step struct{ Error string }
		if e := dec.Decode(&step); e != nil {
			return e
		}
		if step.Error != "" {
			return errors.New(step.Error)
		}
	}
	return nil
}
func (i installation) k8sStart(c *commandClient, cfg k8sConfig, use bool) error {
	started := time.Now()
	node, e := c.k8sNode()
	if e != nil {
		return e
	}
	if node != nil && !node.State.Running {
		made, has := node.Config.Labels[k8sConfigLabel]
		stale := has && made != k8sMade(cfg) || !has && node.port() != cfg.port()
		if stale || !k8sPortFree(node.port(), cfg.LAN) {
			if e := c.request("DELETE", "/containers/"+k8sName+"?force=1", nil, nil); e != nil {
				return e
			}
			node = nil
		}
	}
	if node == nil {
		port, e := k8sPickPort(cfg.port(), cfg.LAN)
		if e != nil {
			return e
		}
		if port != cfg.port() {
			fmt.Printf("port %d is in use; the API server takes %d\n", cfg.port(), port)
		}
		fmt.Printf("pulling %s…\n", k8sImage)
		if e := c.pullImage(k8sImage); e != nil {
			return e
		}
		if e := c.request("POST", "/containers/create?name="+k8sName, k8sSpec(cfg, port), nil); e != nil {
			return e
		}
	}
	if e := machineAction(c, k8sName, "start"); e != nil {
		return e
	}
	written, e := k8sHostNames(c)
	if e != nil {
		fmt.Fprintf(os.Stderr, "pods may not resolve host.docker.internal: %v\n", e)
	}
	fmt.Println("waiting for the node…")
	deadline := time.Now().Add(180 * time.Second)
	for {
		code, out, e := c.exec(k8sName, []string{"timeout", "-s", "KILL", "20", "kubectl", "get", "nodes", "--no-headers"})
		fields := strings.Fields(out)
		if e == nil && code == 0 && len(fields) > 1 && fields[1] == "Ready" {
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the node was not ready after 3 minutes; see fabo logs %s", k8sName)
		}
		time.Sleep(time.Second)
	}
	if written {
		if e := k8sRestartDNS(c); e != nil {
			fmt.Fprintf(os.Stderr, "pods may not resolve host.docker.internal until CoreDNS restarts: %v\n", e)
		}
	}
	node, e = c.k8sNode()
	if e != nil {
		return e
	}
	if node == nil {
		return errors.New("Kubernetes node disappeared")
	}
	if e := i.k8sSync(c, use, node.port()); e != nil {
		return e
	}
	fmt.Printf("Kubernetes is ready in %.0f s, API on 127.0.0.1:%d: kubectl --context %s get nodes\n", time.Since(started).Seconds(), node.port(), i.names.context)
	return nil
}
func (i installation) k8sSync(c *commandClient, use bool, port int) error {
	code, raw, e := c.exec(k8sName, []string{"cat", "/etc/rancher/k3s/k3s.yaml"})
	if e != nil {
		return e
	}
	if code != 0 {
		return errors.New("could not read the cluster's kubeconfig")
	}
	ours, e := kubeFromK3s(raw, i.names.context, port)
	if e != nil {
		return e
	}
	if e := writePrivateFile(filepath.Join(i.root(), "kubeconfig"), []byte(ours)); e != nil {
		return e
	}
	changed, e := updateKubeconfig(i.kubePath(), ours, i.names.context, use, false)
	if e != nil {
		return e
	}
	if changed {
		fmt.Printf("kubectl context `%s` written to %s\n", i.names.context, i.kubePath())
	}
	return nil
}
func k8sHostNames(c *commandClient) (bool, error) {
	// Match the guest's host address, not the node's bridge gateway.
	manifest := `# Written by Fabo: pods resolve the host's names.
apiVersion: v1
kind: ConfigMap
metadata:
  name: coredns-custom
  namespace: kube-system
data:
  fabo-host.server: |
    host.docker.internal:53 host.fabo.internal:53 gateway.docker.internal:53 {
        hosts {
            ` + guestwire.HostRelayAddress + ` host.docker.internal host.fabo.internal gateway.docker.internal
        }
    }
`
	script := `set -e; d=/var/lib/rancher/k3s/server/manifests; f=$d/fabo-host-names.yaml; mkdir -p $d; printf '%s' "$1" > $d/.fabo-host-names.tmp; if cmp -s $d/.fabo-host-names.tmp $f; then rm -f $d/.fabo-host-names.tmp; else mv $d/.fabo-host-names.tmp $f; echo written; fi`
	code, out, e := c.exec(k8sName, []string{"sh", "-c", script, "sh", manifest})
	if e != nil {
		return false, e
	}
	if code != 0 {
		return false, fmt.Errorf("could not write host names manifest: %s", out)
	}
	return strings.Contains(out, "written"), nil
}
func k8sRestartDNS(c *commandClient) error {
	script := `k='kubectl -n kube-system'; for i in $(seq 90); do if $k get configmap coredns-custom -o name >/dev/null 2>&1 && $k get deployment coredns -o name >/dev/null 2>&1; then exec $k rollout restart deployment coredns; fi; sleep 1; done; echo 'the coredns-custom ConfigMap was not applied in 90 s'; exit 1`
	code, out, e := c.exec(k8sName, []string{"timeout", "-s", "KILL", "110", "sh", "-c", script})
	if e != nil {
		return e
	}
	if code != 0 {
		return errors.New(strings.TrimSpace(out))
	}
	return nil
}
