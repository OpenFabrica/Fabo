//go:build linux || darwin

package localengine

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/OpenFabrica/Fabo/engine/internal/guestwire"
	"github.com/OpenFabrica/Fabo/engine/internal/hostagent"
)

func TestCustomNetworkRestoresAddressReservations(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	state, err := OpenState(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	images := map[string]Image{"local": {Cmd: []string{"/bin/test"}, Layers: []hostagent.Layer{{Digest: "layer"}}}}
	e := &Engine{state: state, containers: map[string]*container{}, images: images}
	create := httptest.NewRecorder()
	e.createNetworkHTTP(create, httptest.NewRequest(http.MethodPost, "/networks/create", strings.NewReader(`{
		"Name":"example", "Options":{"example.driver":"test"},
		"IPAM":{"Options":{"example.ipam":"test"}, "Config":[{"Subnet":"10.51.0.0/24", "IPRange":"10.51.0.128/30",
		"Gateway":"10.51.0.128", "AuxiliaryAddresses":{"router":"10.51.0.129"}}]}
	}`)))
	if create.Code != http.StatusCreated {
		t.Fatalf("network create: %d %s", create.Code, create.Body.String())
	}
	n := e.networks["example"]
	ip := netip.MustParseAddr("10.51.0.20")
	mac, err := net.ParseMAC("02:42:0a:33:00:14")
	if err != nil {
		t.Fatal(err)
	}
	config := Config{Image: "local", network: n.Name, networkEndpoints: map[string]endpointConfig{n.Name: {ip: ip, mac: mac, aliases: []string{"api"}}}, Labels: map[string]string{"project": "alpha"}}
	id, err := e.create("first", config)
	if err != nil {
		t.Fatal(err)
	}
	dynamic, err := e.create("dynamic", Config{Image: "local", network: n.Name})
	if err != nil {
		t.Fatal(err)
	}
	restored, err := New(t.Context(), nil, images, state, VMOptions{})
	if err != nil {
		t.Fatal(err)
	}
	c := restored.containers[id]
	if c.networkID != n.ID || c.run.Network.IP != ip.As4() || !restored.matchesContainer(c, map[string][]string{"label": {"project=alpha"}}, nil) {
		t.Fatalf("restored container: %+v", c)
	}
	n = restored.networks[n.Name]
	if n.Options["example.driver"] != "test" || n.IPAMOptions["example.ipam"] != "test" {
		t.Fatalf("restored network options: %+v", n)
	}
	gotMAC, err := c.run.Network.MAC()
	if err != nil || !c.run.Network.StaticIPv4 || gotMAC.String() != mac.String() {
		t.Fatalf("restored fixed endpoint: %+v, %v", c.run.Network, err)
	}
	if got := restored.containers[dynamic].networkAddress().String(); got != "10.51.0.130" {
		t.Fatalf("restored dynamic address: %s", got)
	}
	for _, reserved := range []string{ip.String(), "10.51.0.128", "10.51.0.129"} {
		if _, err := restored.allocateNetwork(n, netip.MustParseAddr(reserved)); !errors.Is(err, errConflict) {
			t.Fatalf("address %s must stay reserved: %v", reserved, err)
		}
	}
	next, err := restored.allocateNetwork(n, netip.Addr{})
	if got := netip.AddrFrom4(next.IP).String(); err != nil || got != "10.51.0.131" {
		t.Fatalf("next address: %s, %v", got, err)
	}
}

// A failed host record write must undo an endpoint already added in the guest.
func TestLiveConnectRollsBackAfterSaveFailure(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	state, err := OpenState(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	e := &Engine{ctx: t.Context(), state: state, containers: map[string]*container{},
		images: map[string]Image{"local": {Cmd: []string{"/bin/test"}, Layers: []hostagent.Layer{{Digest: "layer"}}}},
	}
	n := dockerNetwork{Name: "extra"}
	if err := e.createNetwork(&n); err != nil {
		t.Fatal(err)
	}
	id, err := e.create("target", Config{Image: "local", network: "bridge"})
	if err != nil {
		t.Fatal(err)
	}
	c := e.containers[id]
	c.state = "running"
	c.nextAttempt()
	state.dir = filepath.Join(dir, "unavailable")
	host, guest := net.Pipe()
	e.session = guestwire.NewSession(host, nil)
	defer e.session.Close()
	_ = guest.SetDeadline(time.Now().Add(3 * time.Second))
	peer := make(chan error, 1)
	go func() {
		defer guest.Close()
		var address [4]byte
		for _, kind := range []guestwire.Kind{guestwire.NetAttachMessage, guestwire.NetDetachMessage} {
			frame, err := guestwire.ReadFrame(guest, nil)
			if err != nil {
				peer <- err
				return
			}
			target, ifname, network, err := guestwire.DecodeNetworkChange(frame)
			if err != nil || frame.Kind != kind || target != id || ifname != "eth1" || network.Bridge != n.Bridge ||
				kind == guestwire.NetDetachMessage && network.IP != address {
				peer <- fmt.Errorf("network rollback frame: %+v, %v", frame, err)
				return
			}
			address = network.IP
			if err := guestwire.WriteFrame(guest, guestwire.Frame{Kind: guestwire.AckMessage}); err != nil {
				peer <- err
				return
			}
		}
		peer <- nil
	}()
	if err := e.changeNetwork(t.Context(), id, n.ID, endpointConfig{}, true); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("connect: %v", err)
	}
	if err := <-peer; err != nil {
		t.Fatal(err)
	}
	if endpoints := c.endpoints(); len(endpoints) != 1 || endpoints[0].Name != "bridge" {
		t.Fatalf("failed connect changed host metadata: %+v", endpoints)
	}
}

func TestHostNetworkSurvivesStateRestore(t *testing.T) {
	dir := t.TempDir()
	e := &Engine{
		images:     map[string]Image{"local": {Cmd: []string{"/bin/test"}}},
		containers: make(map[string]*container), state: &State{dir: dir},
	}
	if _, err := e.create("mixed", Config{Image: "local", network: "host",
		networkEndpoints: map[string]endpointConfig{"bridge": {}},
	}); !errors.Is(err, errInvalid) {
		t.Fatalf("host network accepted a bridge: %v", err)
	}
	id, err := e.create("hosted", Config{Image: "local", network: "host",
		networkEndpoints: map[string]endpointConfig{"host": {networkID: "host"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	restored, err := e.state.restore()
	if err != nil {
		t.Fatal(err)
	}
	c := restored[id]
	if c == nil || c.networkMode() != "host" || !c.networkAddress().IsLoopback() {
		t.Fatal("restored container lost its host network")
	}
	endpoints := c.endpoints()
	if len(endpoints) != 1 || endpoints[0].ID != "host" || endpoints[0].Network.Bridge != "" {
		t.Fatalf("unexpected host network endpoint: %+v", endpoints)
	}
	if err := e.changeNetwork(t.Context(), id, "bridge", endpointConfig{}, true); !errors.Is(err, errInvalid) {
		t.Fatalf("attached a host-network container to a bridge: %v", err)
	}
}

func TestSharedNetworkKeepsCanonicalTarget(t *testing.T) {
	e := &Engine{
		images:     map[string]Image{"local": {Cmd: []string{"/bin/test"}}},
		containers: make(map[string]*container), state: &State{dir: t.TempDir()},
	}
	owner, err := e.create("owner", Config{Image: "local", network: "bridge"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = e.create("missing", Config{Image: "local", network: "container:nope"})
	if !errors.Is(err, errMissing) || !strings.Contains(err.Error(), "No such container: nope") {
		t.Fatalf("missing shared network target: %v", err)
	}
	id, err := e.create("joiner", Config{Image: "local", network: "container:" + owner[:12]})
	if err != nil {
		t.Fatal(err)
	}
	e.containers, err = e.state.restore()
	if err != nil {
		t.Fatal(err)
	}
	c := e.containers[id]
	c.setEndpoints(c.endpoints())
	if c.networkMode() != "container:"+owner || len(c.endpoints()) != 0 || c.run.Flags&guestwire.RunNetworkNone != 0 {
		t.Fatalf("invalid restored network: %s %+v", c.networkMode(), c.run)
	}
	waiter := c.nextAttempt()
	if err := e.startContainer(c, false); !errors.Is(err, errConflict) || c.state != "exited" ||
		!strings.Contains(err.Error(), "cannot join network of a non running container") {
		t.Fatalf("starting without a running target: %s, %v", c.state, err)
	}
	select {
	case <-waiter.done:
		if !errors.Is(waiter.failure, errConflict) {
			t.Fatalf("wrong failure for waiters: %v", waiter.failure)
		}
	default:
		t.Fatal("waiter still attached after the start failed")
	}
	if err := e.setPorts(id, []portBinding{{ContainerPort: 80, Protocol: "tcp", HostPort: 8080}}); !errors.Is(err, errInvalid) {
		t.Fatalf("published ports on a shared network: %v", err)
	}
}

func TestIPv6OnlyAddressPoolSurvivesRestore(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	state, err := OpenState(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()

	images := map[string]Image{"local": {Cmd: []string{"/bin/test"}}}
	e := &Engine{state: state, images: images, containers: map[string]*container{}}
	pool := &ipv6Pool{
		Subnet: netip.MustParsePrefix("fd00:42::/64"), Gateway: netip.MustParseAddr("fd00:42::1"),
		IPRange:   netip.MustParsePrefix("fd00:42::8/126"),
		Auxiliary: map[string]netip.Addr{"reserved": netip.MustParseAddr("fd00:42::8")},
	}
	n := dockerNetwork{Name: "v6", NoIPv4: true, IPv6: pool}
	if err := e.createNetwork(&n); err != nil {
		t.Fatal(err)
	}
	id, err := e.create("first", Config{Image: "local", network: n.Name})
	if err != nil {
		t.Fatal(err)
	}
	restored, err := New(t.Context(), nil, images, state, VMOptions{})
	if err != nil {
		t.Fatal(err)
	}
	addr := netip.MustParseAddr("fd00:42::9")
	if got := restored.containers[id].networkAddress(); got != addr {
		t.Fatalf("restored endpoint = %s, want %s", got, addr)
	}
	persisted := restored.networks[n.Name]
	if _, err := restored.allocateIPv6(persisted, addr); !errors.Is(err, errConflict) {
		t.Fatalf("restored reservation: %v", err)
	}
	if got, err := restored.allocateIPv6(persisted, netip.Addr{}); err != nil || got != addr.Next() {
		t.Fatalf("next address = %s, %v", got, err)
	}
}

func TestNetworkConnectRejectsLinkCycles(t *testing.T) {
	e := &Engine{
		images:     map[string]Image{"local": {Cmd: []string{"/bin/test"}}},
		containers: make(map[string]*container), state: &State{dir: t.TempDir()},
	}
	for _, name := range []string{"left", "right"} {
		if err := e.createNetwork(&dockerNetwork{Name: name}); err != nil {
			t.Fatal(err)
		}
		if _, err := e.create(name, Config{Image: "local", network: "none"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.changeNetwork(t.Context(), "left", "left", endpointConfig{links: []string{"right:peer"}}, true); err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{"right", "left"} {
		err := e.changeNetwork(t.Context(), "right", "right", endpointConfig{links: []string{target + ":peer"}}, true)
		if !errors.Is(err, errConflict) || !strings.Contains(err.Error(), "startup dependency cycle") {
			t.Fatalf("link to %s: %v", target, err)
		}
	}
	right, err := e.find("right")
	if err != nil || len(right.endpoints()) != 0 || len(right.links) != 0 {
		t.Fatalf("rejected link changed the container: %+v, %v", right, err)
	}
}

func TestDefaultBridgeIPv6ToggleRetainsReservation(t *testing.T) {
	t.Setenv("FABO_BRIDGE_IPV6", "0")
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	state, err := OpenState(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	images := map[string]Image{"local": {Cmd: []string{"/bin/test"}}}
	e := &Engine{state: state, images: images, containers: map[string]*container{}}
	id, err := e.create("existing", Config{Image: "local", network: "bridge"})
	if err != nil {
		t.Fatal(err)
	}

	reopen := func(value string) *Engine {
		t.Helper()
		t.Setenv("FABO_BRIDGE_IPV6", value)
		engine, err := New(t.Context(), nil, images, state, VMOptions{})
		if err != nil {
			t.Fatal(err)
		}
		return engine
	}
	on := reopen("1")
	addr, _, err := on.containers[id].run.Network.IPv6()
	if err != nil || !addr.IsValid() || !on.bridgeNetwork().IPv6.Subnet.Contains(addr.Addr()) {
		t.Fatalf("missing address on an existing container: %s, %v", addr, err)
	}
	off := reopen("0")
	if off.bridgeNetwork().IPv6 != nil {
		t.Fatal("disabled bridge advertised an IPv6 pool")
	}
	if live, _, _ := off.liveNetwork(off.containers[id].run.Network).IPv6(); live.IsValid() {
		t.Fatalf("disabled bridge still sends %s to the guest", live)
	}
	if off.freeIPv6Subnet(addr.Masked()) {
		t.Fatal("custom networks can take a dormant bridge subnet")
	}
	restarted := reopen("true")
	if live, _, err := restarted.liveNetwork(restarted.containers[id].run.Network).IPv6(); err != nil || live != addr {
		t.Fatalf("IPv6 address after re-enabling = %s, %v, want %s", live, err, addr)
	}
}
