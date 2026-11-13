//go:build linux || darwin

package localengine

import (
	"net"
	"strings"

	"golang.org/x/net/dns/dnsmessage"
)

// serveDomainDNS answers the scoped queries sent by Kiri's macOS resolver
// rule. It never forwards queries to other DNS servers.
func serveDomainDNS(socket net.PacketConn) {
	var packet [4096]byte
	for {
		n, from, err := socket.ReadFrom(packet[:])
		if err != nil {
			return
		}
		if reply := domainDNSAnswer(packet[:n]); reply != nil {
			_, _ = socket.WriteTo(reply, from)
		}
	}
}

func domainDNSAnswer(packet []byte) []byte {
	var query dnsmessage.Message
	if err := query.Unpack(packet); err != nil || query.Response || query.OpCode != 0 || len(query.Questions) != 1 || len(query.Answers) != 0 {
		return nil
	}
	q := query.Questions[0]
	name := strings.ToLower(q.Name.String())
	reply := dnsmessage.Message{
		Header:    dnsmessage.Header{ID: query.ID, Response: true, Authoritative: true, RecursionDesired: query.RecursionDesired},
		Questions: query.Questions,
	}
	if q.Class != dnsmessage.ClassINET || name != "kiri.local." && !strings.HasSuffix(name, ".kiri.local.") {
		reply.RCode = dnsmessage.RCodeRefused
	} else if q.Type == dnsmessage.TypeA {
		reply.Answers = []dnsmessage.Resource{{
			Header: dnsmessage.ResourceHeader{Name: q.Name, Type: q.Type, Class: q.Class, TTL: 60},
			Body:   &dnsmessage.AResource{A: [4]byte{127, 0, 0, 1}},
		}}
	}
	encoded, _ := reply.Pack()
	return encoded
}
