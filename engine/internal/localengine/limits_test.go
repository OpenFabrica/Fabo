//go:build linux || darwin

package localengine

import (
	"encoding/json"
	"testing"

	"github.com/OpenFabrica/FabriBay/engine/internal/guestwire"
)

func TestDockerLimits(t *testing.T) {
	var host map[string]json.RawMessage
	if err := json.Unmarshal([]byte(`{"Memory":67108864,"NanoCpus":500000000,"PidsLimit":32,"ShmSize":8388608,"CpuShares":1024,"NetworkMode":"none"}`), &host); err != nil {
		t.Fatal(err)
	}
	got, err := parseLimits(host)
	want := guestwire.Limits{MemoryMax: 67108864, CPUQuota: 50000, CPUPeriod: 100000, CPUWeight: 39, PIDsMax: 32, ShmSize: 8388608}
	if err != nil || got != want || len(host) != 1 || host["NetworkMode"] == nil {
		t.Fatalf("limits: %+v, remaining=%v, err=%v", got, host, err)
	}
	if _, err := parseLimits(map[string]json.RawMessage{"NanoCpus": []byte("500000000"), "CpuQuota": []byte("50000")}); err == nil {
		t.Fatal("accepted conflicting CPU limits")
	}
}
