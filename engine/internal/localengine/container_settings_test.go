//go:build linux || darwin

package localengine

import (
	"net"
	"net/netip"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/OpenFabrica/FabriBay/engine/internal/guestwire"
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
