//go:build linux || darwin

package main

import (
	"encoding/json"
	"net"
	"net/http"
	"strings"
	"testing"
)

func TestK8sSettingsAndContainerContract(t *testing.T) {
	for _, args := range [][]string{{"--lan"}, {"--lan", "maybe"}, {"--port", "80"}, {"--port", "65536"}, {"--tls-san", "a..b"}, {"--tls-san", "--bad"}, {"--tls-san", "x/y"}, {"--unknown", "x"}} {
		if _, e := k8sSettings(k8sConfig{}, args); e == nil {
			t.Fatal(args)
		}
	}
	cfg, e := k8sSettings(k8sConfig{}, []string{"--port", "6448", "--lan", "on", "--tls-san", "my-mac.local,::1,*.example.com"})
	if e != nil {
		t.Fatal(e)
	}
	spec := k8sSpec(cfg, 6450)
	raw, _ := json.Marshal(spec)
	for _, want := range []string{`"Privileged":true`, `"HostIp":"0.0.0.0"`, `"HostPort":"6450"`, `"dev.kiri.k8s":"1"`, `kiri-k8s:/var/lib/rancher/k3s`, `my-mac.local`} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("missing %s in %s", want, raw)
		}
	}
	var node k8sNode
	_ = json.Unmarshal([]byte(`{"Config":{"Labels":{"dev.kiri.k8s.port":"6448"}},"NetworkSettings":{"Ports":{"6443/tcp":[{"HostPort":"6450"}]}}}`), &node)
	if node.port() != 6450 {
		t.Fatal(node)
	}
	node.NetworkSettings.Ports = nil
	if node.port() != 6448 {
		t.Fatal(node)
	}
	listener, e := net.Listen("tcp4", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	defer listener.Close()
	port := listener.Addr().(*net.TCPAddr).Port
	if k8sPortFree(port, false) {
		t.Fatal("busy port accepted")
	}
	if port < 65535 {
		next, e := k8sPickPort(port, false)
		if e != nil || next <= port {
			t.Fatal(next, e)
		}
	}
}
func TestK8sInspectDoesNotHideServerFailure(t *testing.T) {
	for _, status := range []int{404, 500} {
		c := machineTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
			w.Write([]byte(`{"message":"broken"}`))
		})
		node, e := c.k8sNode()
		if status == 404 && (node != nil || e != nil) {
			t.Fatal(node, e)
		}
		if status == 500 && (e == nil || !strings.Contains(e.Error(), "broken")) {
			t.Fatal(e)
		}
	}
}
