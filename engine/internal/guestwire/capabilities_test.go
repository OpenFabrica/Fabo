package guestwire

import "testing"

func TestCapabilityMaskMatchesRustSemantics(t *testing.T) {
	tests := []struct {
		add, drop []string
		want      uint64
	}{
		{nil, nil, 0},
		{[]string{" cap_net_admin "}, nil, DefaultCapabilities | 1<<12},
		{nil, []string{"ALL"}, CapabilityNone},
		{[]string{"NET_ADMIN"}, []string{"ALL"}, 1 << 12},
		{[]string{"CHOWN"}, []string{"CHOWN"}, DefaultCapabilities},
		{[]string{"all"}, nil, AllCapabilities},
	}
	for _, tt := range tests {
		got, err := CapabilityMask(tt.add, tt.drop)
		if err != nil || got != tt.want {
			t.Fatalf("%v / %v: %x, %v, want %x", tt.add, tt.drop, got, err, tt.want)
		}
	}
	if _, err := CapabilityMask([]string{"not-a-capability"}, nil); err == nil {
		t.Fatal("accepted unknown capability")
	}
	if err := ValidateCapabilities(1 << 50); err == nil {
		t.Fatal("accepted unknown capability bit")
	}
}
