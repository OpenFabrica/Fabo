package domaindns

import (
	"testing"

	"golang.org/x/net/dns/dnsmessage"
)

func TestDomainDNSAnswersOnlyLocalNames(t *testing.T) {
	for _, tc := range []struct {
		name  string
		kind  dnsmessage.Type
		code  dnsmessage.RCode
		wantA bool
	}{
		{"API.Sample.Fabo.Local.", dnsmessage.TypeA, dnsmessage.RCodeSuccess, true},
		{"api.fabo.local.", dnsmessage.TypeAAAA, dnsmessage.RCodeSuccess, false},
		{"notfabo.local.", dnsmessage.TypeA, dnsmessage.RCodeRefused, false},
		{"fabo.local.example.", dnsmessage.TypeA, dnsmessage.RCodeRefused, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			query := dnsmessage.Message{
				Header:    dnsmessage.Header{ID: 42, RecursionDesired: true},
				Questions: []dnsmessage.Question{{Name: dnsmessage.MustNewName(tc.name), Type: tc.kind, Class: dnsmessage.ClassINET}},
			}
			packet, err := query.Pack()
			if err != nil {
				t.Fatal(err)
			}
			var reply dnsmessage.Message
			if err := reply.Unpack(Answer(packet)); err != nil {
				t.Fatal(err)
			}
			if !reply.Response || reply.ID != query.ID || reply.RCode != tc.code || reply.RecursionAvailable {
				t.Fatalf("unexpected response header: %+v", reply.Header)
			}
			if !tc.wantA {
				if len(reply.Answers) != 0 {
					t.Fatalf("unexpected answers: %+v", reply.Answers)
				}
				return
			}
			if len(reply.Answers) != 1 {
				t.Fatalf("expected one answer, got %d", len(reply.Answers))
			}
			a, ok := reply.Answers[0].Body.(*dnsmessage.AResource)
			if !ok || a.A != [4]byte{127, 0, 0, 1} {
				t.Fatalf("unexpected A answer: %+v", reply.Answers[0].Body)
			}
		})
	}
}

// TestAnswerSurvivesTruncatedQueries feeds every prefix of a valid query
// and some garbage: anyone on the machine can send to the answerer, and a
// malformed packet must be dropped, not crash the process.
func TestAnswerSurvivesTruncatedQueries(t *testing.T) {
	query := dnsmessage.Message{
		Header:    dnsmessage.Header{ID: 7},
		Questions: []dnsmessage.Question{{Name: dnsmessage.MustNewName("web.fabo.local."), Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET}},
	}
	packet, err := query.Pack()
	if err != nil {
		t.Fatal(err)
	}
	for i := range packet {
		if Answer(packet[:i]) != nil {
			t.Fatalf("answered a %d-byte prefix", i)
		}
	}
	long := append(append([]byte{0, 1, 0, 0, 0, 1, 0, 0, 0, 0, 0, 0, 64}, make([]byte, 64)...), 0, 0, 1, 0, 1)
	if Answer(long) != nil {
		t.Fatal("answered a 64-byte label")
	}
	reply := dnsmessage.Message{Header: dnsmessage.Header{ID: 7, Response: true}, Questions: query.Questions}
	if b, _ := reply.Pack(); Answer(b) != nil {
		t.Fatal("answered a response")
	}
}
