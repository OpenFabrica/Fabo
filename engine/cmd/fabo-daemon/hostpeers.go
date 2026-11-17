//go:build linux || (darwin && cgo)

package main

// Which container is behind a connection the host relay made: the relay
// connects to the Mac's loopback from an address of its own, so a server
// there sees every container as 127.0.0.1. The relay records the address
// it connected from with the container's, and GET /fabo/debug/peers
// answers for the Debug page. Without a VM relay (Linux) the table stays
// empty and servers see containers' own addresses.

import (
	"encoding/json"
	"net/http"
	"net/netip"
	"sync"

	"github.com/OpenFabrica/Fabo/engine/internal/localengine"
)

// hostPeerKey names a relayed connection by protocol and the loopback
// address it came from.
type hostPeerKey struct {
	udp   bool
	local string
}

// hostPeerTable holds the relayed connections that are open.
type hostPeerTable struct {
	mu    sync.Mutex
	peers map[hostPeerKey]netip.AddrPort
}

var hostPeers = &hostPeerTable{peers: map[hostPeerKey]netip.AddrPort{}}

// add records a connection the relay opened and returns its removal.
func (t *hostPeerTable) add(udp bool, local string, source netip.AddrPort) func() {
	key := hostPeerKey{udp, local}
	t.mu.Lock()
	t.peers[key] = source
	t.mu.Unlock()
	return func() {
		t.mu.Lock()
		if t.peers[key] == source {
			delete(t.peers, key)
		}
		t.mu.Unlock()
	}
}

// lookup finds the container address behind a loopback address.
func (t *hostPeerTable) lookup(udp bool, local string) (netip.AddrPort, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	source, ok := t.peers[hostPeerKey{udp, local}]
	return source, ok
}

// hostPeerInfo is one answer of GET /fabo/debug/peers.
type hostPeerInfo struct {
	Source    string `json:"source"`
	Container string `json:"container,omitempty"`
}

// servePeers is GET /fabo/debug/peers?proto=tcp|udp&addr=…: for each
// loopback address the relay connected from, the container address and,
// when the engine runs, the container's name.
func servePeers(w http.ResponseWriter, r *http.Request, table *hostPeerTable, engine *localengine.Engine) {
	w.Header().Set("X-Fabo-Debug", "1")
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	udp := r.URL.Query().Get("proto") == "udp"
	out := map[string]hostPeerInfo{}
	for _, addr := range r.URL.Query()["addr"] {
		source, ok := table.lookup(udp, addr)
		if !ok {
			continue
		}
		info := hostPeerInfo{Source: source.String()}
		if engine != nil {
			info.Container = engine.ContainerAt(source.Addr())
		}
		out[addr] = info
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}
