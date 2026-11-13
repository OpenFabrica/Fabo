//go:build linux || darwin

package localengine

import (
	"encoding/json"
	"fmt"

	"github.com/libracoder/kiri/engine/internal/guestwire"
)

func parseDeviceOptions(host map[string]json.RawMessage) (guestwire.DeviceOptions, error) {
	var devices guestwire.DeviceOptions
	if raw := host["Devices"]; len(raw) != 0 {
		if err := json.Unmarshal(raw, &devices.Mappings); err != nil {
			return devices, fmt.Errorf("%w: invalid Devices", errInvalid)
		}
		delete(host, "Devices")
	}
	if raw := host["DeviceCgroupRules"]; len(raw) != 0 {
		if err := json.Unmarshal(raw, &devices.Rules); err != nil {
			var rule string
			if err := json.Unmarshal(raw, &rule); err != nil {
				return devices, fmt.Errorf("%w: invalid DeviceCgroupRules", errInvalid)
			}
			devices.Rules = []string{rule}
		}
		delete(host, "DeviceCgroupRules")
	}
	for i, mapping := range devices.Mappings {
		normalized, err := mapping.Normalize()
		if err != nil {
			return devices, fmt.Errorf("%w: %v", errInvalid, err)
		}
		devices.Mappings[i] = normalized
	}
	if err := devices.Validate(); err != nil {
		return devices, fmt.Errorf("%w: %v", errInvalid, err)
	}
	return devices, nil
}
