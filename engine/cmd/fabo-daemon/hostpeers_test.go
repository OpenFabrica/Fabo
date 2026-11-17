//go:build linux || (darwin && cgo)

package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
)

func TestHostPeersAnswerForOpenRelays(t *testing.T) {
	table := &hostPeerTable{peers: map[hostPeerKey]netip.AddrPort{}}
	source := netip.MustParseAddrPort("172.18.0.3:40512")
	remove := table.add(false, "127.0.0.1:53012", source)
	table.add(true, "127.0.0.1:61000", netip.MustParseAddrPort("172.18.0.4:5514"))

	ask := func(query string) map[string]hostPeerInfo {
		w := httptest.NewRecorder()
		servePeers(w, httptest.NewRequest(http.MethodGet, "/fabo/debug/peers?"+query, nil), table, nil)
		if w.Code != http.StatusOK || w.Header().Get("X-Fabo-Debug") != "1" {
			t.Fatalf("status %d", w.Code)
		}
		var out map[string]hostPeerInfo
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	got := ask("proto=tcp&addr=127.0.0.1:53012&addr=127.0.0.1:1")
	if len(got) != 1 || got["127.0.0.1:53012"].Source != "172.18.0.3:40512" {
		t.Fatalf("tcp peers %v", got)
	}
	// UDP and TCP are told apart.
	if got := ask("proto=tcp&addr=127.0.0.1:61000"); len(got) != 0 {
		t.Fatalf("a UDP flow answered for TCP: %v", got)
	}
	if got := ask("proto=udp&addr=127.0.0.1:61000"); got["127.0.0.1:61000"].Source != "172.18.0.4:5514" {
		t.Fatalf("udp peers %v", got)
	}
	remove()
	if got := ask("proto=tcp&addr=127.0.0.1:53012"); len(got) != 0 {
		t.Fatalf("a closed relay still answers: %v", got)
	}
}
