//go:build linux || darwin

package localengine

import (
	"net"
	"net/netip"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/OpenFabrica/Fabo/engine/internal/guestwire"
)

func TestPublishedPortUpdateRestoresAssignedPortOnSaveFailure(t *testing.T) {
	target := netip.MustParseAddr("127.0.0.1")
	service, err := net.ListenTCP("tcp4", net.TCPAddrFromAddrPort(netip.AddrPortFrom(target, 0)))
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	requested := []portBinding{{ContainerPort: service.Addr().(*net.TCPAddr).AddrPort().Port(), Protocol: "tcp", HostIP: "127.0.0.1"}}
	prior, err := reservePorts(requested, target, nil)
	if err != nil {
		t.Fatal(err)
	}
	prior.start()
	c := &container{
		id: "web", name: "web", state: "running", portBindings: requested,
		run:     guestwire.RunSpec{Network: guestwire.Network{IP: target.As4()}},
		attempt: &runAttempt{ports: prior},
	}
	t.Cleanup(func() {
		if c.attempt.ports != nil {
			c.attempt.ports.close()
		}
	})
	e := &Engine{
		containers: map[string]*container{c.id: c},
		state:      &State{dir: filepath.Join(t.TempDir(), "missing")},
	}
	next := []portBinding{{ContainerPort: 81, Protocol: "tcp", HostIP: "127.0.0.1"}}
	if err := e.setPorts(c.id, next); err == nil {
		t.Fatal("update unexpectedly saved")
	}
	if c.attempt.ports == nil || !slices.Equal(c.portBindings, requested) || !slices.Equal(c.attempt.ports.bound, prior.bound) {
		t.Fatal("failed update lost the requested or assigned port")
	}
	conn, err := net.DialTimeout("tcp", netip.AddrPortFrom(target, prior.bound[0].HostPort).String(), time.Second)
	if err != nil {
		t.Fatalf("old listener was not restored: %v", err)
	}
	_ = conn.Close()
}

func TestListeningPortsDistinguishLoopbackAndIPv6(t *testing.T) {
	rows := []string{
		"0: 00000000:0050 00000000:0000 0A",
		"1: 0100007F:0050 00000000:0000 0A",
		"2: 0100007F:0051 00000000:0000 0A",
		"3: 00000000000000000000000000000000:0052 00000000:0000 0A",
		"4: 00000000000000000000000001000000:0053 00000000:0000 0A",
		"5: 00000000:0054 00000000:0000 01",
	}
	tcp, local := listeningPorts(rows, true, false)
	if !slices.Equal(tcp, []uint16{80, 82}) || !slices.Equal(local, []uint16{81, 83}) {
		t.Fatalf("bridge listeners: tcp %v, local %v", tcp, local)
	}
	tcp, local = listeningPorts(rows, false, false)
	if len(tcp) != 0 || !slices.Equal(local, []uint16{80, 81, 82, 83}) {
		t.Fatalf("isolated listeners: tcp %v, local %v", tcp, local)
	}
}

// Names typed in Settings are normalized, take effect for the router at
// once, refuse a name another container answers to, and clear when empty.
func TestSetDomainUpdatesTheRouterAndRefusesTakenNames(t *testing.T) {
	web := &container{id: "web0123456789", name: "web", labels: map[string]string{"com.docker.compose.project": "shop"}}
	db := &container{id: "db0123456789", name: "db"}
	e := &Engine{containers: map[string]*container{web.id: web, db.id: db}}

	names, err := domainNames(" Shop.fabo.local, api.shop ,shop ")
	if err != nil || !slices.Equal(names, []string{"shop", "api.shop"}) {
		t.Fatalf("names %v, %v", names, err)
	}
	if _, err := domainNames("bad name"); err == nil {
		t.Fatal("a name with a space was accepted")
	}
	before := web.labels
	if err := e.setDomain("web", names); err != nil {
		t.Fatal(err)
	}
	if web.labels[domainLabel] != "shop,api.shop" || before[domainLabel] != "" || web.labels["com.docker.compose.project"] != "shop" {
		t.Fatalf("labels %v (old map %v)", web.labels, before)
	}
	if !matchesDomain(web, "api.shop") {
		t.Fatal("router does not see the new name")
	}
	if err := e.setDomain("db", []string{"api.shop"}); err == nil {
		t.Fatal("a name of another container was accepted")
	}
	if err := e.setDomain("db", []string{"web"}); err == nil {
		t.Fatal("another container's own name was accepted")
	}
	if err := e.setDomain("web", nil); err != nil {
		t.Fatal(err)
	}
	if _, ok := web.labels[domainLabel]; ok || matchesDomain(web, "api.shop") {
		t.Fatalf("names not cleared: %v", web.labels)
	}
}
