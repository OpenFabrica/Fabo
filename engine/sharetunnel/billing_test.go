package sharetunnel

import (
	"testing"

	"github.com/OpenFabrica/Fabo/engine/sharewire"
)

// TestQuotaGoodbyeAndRefusedClassifyOnlyThePrefix distinguishes billing recovery from permanent refusals.
func TestQuotaGoodbyeAndRefusedClassifyOnlyThePrefix(t *testing.T) {
	s := &session{}
	for _, m := range []sharewire.Msg{sharewire.Refused{Reason: sharewire.QuotaExhaustedPrefix + " limit"}, sharewire.Goodbye{Reason: sharewire.QuotaExhaustedPrefix + " traffic"}} {
		e, done := s.handle(m)
		if !done || e.kind != endWaiting {
			t.Fatalf("quota frame: %+v", e)
		}
	}
	e, _ := s.handle(sharewire.Refused{Reason: "token revoked"})
	if e.kind != endFinal {
		t.Fatal("permanent refusal became retryable")
	}
}
