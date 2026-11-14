//go:build linux || darwin

package localengine

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/OpenFabrica/FabriBay/engine/internal/guestwire"
)

// parseProcessOptions consumes the HostConfig fields supported by the guest.
func parseProcessOptions(host map[string]json.RawMessage) (guestwire.ProcessOptions, error) {
	var options guestwire.ProcessOptions
	if raw := host["GroupAdd"]; len(raw) != 0 {
		if err := json.Unmarshal(raw, &options.Groups); err != nil {
			var group string
			if err := json.Unmarshal(raw, &group); err != nil {
				return guestwire.ProcessOptions{}, fmt.Errorf("%w: invalid GroupAdd", errInvalid)
			}
			options.Groups = []string{group}
		}
		delete(host, "GroupAdd")
	}
	if raw := host["Ulimits"]; len(raw) != 0 {
		if err := json.Unmarshal(raw, &options.Ulimits); err != nil {
			return guestwire.ProcessOptions{}, fmt.Errorf("%w: invalid Ulimits", errInvalid)
		}
		delete(host, "Ulimits")
	}
	if err := options.Validate(); err != nil {
		return guestwire.ProcessOptions{}, fmt.Errorf("%w: %v", errInvalid, err)
	}
	return options, nil
}

func parseSysctlConfig(host map[string]json.RawMessage) (map[string]string, error) {
	raw := host["Sysctls"]
	if len(raw) == 0 {
		return nil, nil
	}
	var values map[string]json.RawMessage
	if err := json.Unmarshal(raw, &values); err != nil {
		return nil, fmt.Errorf("%w: invalid Sysctls map", errInvalid)
	}
	delete(host, "Sysctls")
	if len(values) == 0 {
		return nil, nil
	}
	settings := make(map[string]string, len(values))
	for key, value := range values {
		text := strings.TrimSpace(string(value))
		if text == "" {
			return nil, fmt.Errorf("%w: missing sysctl value", errInvalid)
		}
		switch text[0] {
		case '"':
			if err := json.Unmarshal(value, &text); err != nil {
				return nil, fmt.Errorf("%w: invalid sysctl value", errInvalid)
			}
		case 't', 'f', '-', '0', '1', '2', '3', '4', '5', '6', '7', '8', '9':
			// Rust's API also accepts numeric and boolean scalars.
		default:
			return nil, fmt.Errorf("%w: invalid sysctl value", errInvalid)
		}
		settings[key] = text
	}
	return settings, nil
}
