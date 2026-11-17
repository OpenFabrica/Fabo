// Package domaindns answers DNS queries for *.fabo.local with 127.0.0.1,
// where the engine's domain router sorts requests out by name. The macOS
// engine serves it behind /etc/resolver/fabo.local; fabo-win serves it on
// Windows' 127.0.0.1:53 behind an NRPT rule. It never forwards a query.
package domaindns

import (
	"strings"

	"golang.org/x/net/dns/dnsmessage"
)

// Suffix is the domain the answers cover.
const Suffix = "fabo.local"

// Answer is the reply to one query packet: 127.0.0.1 for an A record
// under fabo.local, no records for its other types, REFUSED for any other
// name, and nil for anything that is not a single plain query.
func Answer(packet []byte) []byte {
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
	if q.Class != dnsmessage.ClassINET || name != Suffix+"." && !strings.HasSuffix(name, "."+Suffix+".") {
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
