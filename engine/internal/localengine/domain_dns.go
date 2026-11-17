//go:build linux || darwin

package localengine

import (
	"net"

	"github.com/OpenFabrica/Fabo/engine/internal/domaindns"
)

// serveDomainDNS answers the scoped queries sent by Fabo's macOS resolver
// rule. It never forwards queries to other DNS servers.
func serveDomainDNS(socket net.PacketConn) {
	var packet [4096]byte
	for {
		n, from, err := socket.ReadFrom(packet[:])
		if err != nil {
			return
		}
		if reply := domaindns.Answer(packet[:n]); reply != nil {
			_, _ = socket.WriteTo(reply, from)
		}
	}
}
