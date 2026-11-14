//go:build linux || darwin

package localengine

import (
	"encoding/json"
	"fmt"
	"math"
	"math/bits"

	"github.com/OpenFabrica/Fabo/engine/internal/guestwire"
)

func parseLimits(host map[string]json.RawMessage) (guestwire.Limits, error) {
	return mergeLimits(host, guestwire.Limits{})
}

func mergeLimits(host map[string]json.RawMessage, limits guestwire.Limits) (guestwire.Limits, error) {
	read := func(name string) (int64, error) {
		var n int64
		if value, ok := host[name]; ok {
			if err := json.Unmarshal(value, &n); err != nil {
				return 0, fmt.Errorf("%w: invalid %s", errInvalid, name)
			}
			delete(host, name)
		}
		return n, nil
	}
	values := make(map[string]int64)
	for _, key := range []string{"Memory", "NanoCpus", "CpuQuota", "CpuPeriod", "CpuShares", "PidsLimit", "ShmSize"} {
		n, err := read(key)
		if err != nil {
			return limits, err
		}
		values[key] = n
	}
	memory, nano, quota, period, shares, pids, shm := values["Memory"], values["NanoCpus"], values["CpuQuota"], values["CpuPeriod"], values["CpuShares"], values["PidsLimit"], values["ShmSize"]
	if memory < 0 || nano < 0 || quota < -1 || quota > 0 && quota < 1000 ||
		period != 0 && (period < 1000 || period > 1000000) ||
		(shares != 0 && (shares < 2 || shares > 262144)) || pids < -1 || pids > math.MaxUint32 || shm < 0 {
		return limits, fmt.Errorf("%w: resource limit outside supported range", errInvalid)
	}
	if nano != 0 {
		if quota != 0 || period != 0 {
			return limits, fmt.Errorf("%w: NanoCpus conflicts with CpuQuota or CpuPeriod", errInvalid)
		}
		quota, period = nano/10000, 100000
		if quota < 1000 {
			return limits, fmt.Errorf("%w: CPU limit is below the Linux minimum", errInvalid)
		}
	}
	if memory != 0 {
		limits.MemoryMax = uint64(memory)
	}
	if period != 0 {
		limits.CPUPeriod = uint32(period)
	}
	if quota > 0 {
		limits.CPUQuota = uint64(quota)
	} else if quota == -1 {
		limits.CPUQuota = 0
	}
	if shares != 0 {
		limits.CPUWeight = uint32(1 + (shares-2)*9999/262142)
	}
	if pids > 0 {
		limits.PIDsMax = uint32(pids)
	} else if pids == -1 {
		limits.PIDsMax = 0
	}
	if shm != 0 {
		limits.ShmSize = uint64(shm)
	}
	return limits, nil
}

func inspectLimits(host map[string]any, l guestwire.Limits) {
	host["Memory"], host["CpuQuota"], host["CpuPeriod"] = l.MemoryMax, l.CPUQuota, l.CPUPeriod
	var shares uint64
	if l.CPUWeight > 0 {
		shares = 2 + (uint64(l.CPUWeight)-1)*262142/9999
	}
	period := uint64(l.CPUPeriod)
	if period == 0 {
		period = 100000
	}
	hi, lo := bits.Mul64(l.CPUQuota, 1_000_000_000)
	nano := uint64(math.MaxInt64)
	if hi < period {
		nano, _ = bits.Div64(hi, lo, period)
		nano = min(nano, uint64(math.MaxInt64))
	}
	host["NanoCpus"], host["CpuShares"] = nano, shares
	if l.PIDsMax > 0 {
		host["PidsLimit"] = l.PIDsMax
	} else {
		host["PidsLimit"] = nil
	}
	host["ShmSize"] = l.ShmSize
	if l.ShmSize == 0 {
		host["ShmSize"] = 64 << 20
	}
}
