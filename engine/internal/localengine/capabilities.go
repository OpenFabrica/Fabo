//go:build linux || darwin

package localengine

import (
	"encoding/json"
	"fmt"
	"github.com/OpenFabrica/Fabo/engine/internal/guestwire"
)

type capabilityConfig struct{ Add, Drop []string }

func (c capabilityConfig) mask() (uint64, error) { return guestwire.CapabilityMask(c.Add, c.Drop) }
func parseCapabilities(host map[string]json.RawMessage) (capabilityConfig, error) {
	var c capabilityConfig
	for _, f := range []struct {
		name string
		dst  *[]string
	}{{"CapAdd", &c.Add}, {"CapDrop", &c.Drop}} {
		if raw := host[f.name]; len(raw) != 0 {
			if err := json.Unmarshal(raw, f.dst); err != nil {
				var name string
				if err := json.Unmarshal(raw, &name); err != nil {
					return c, fmt.Errorf("%w: invalid %s", errInvalid, f.name)
				}
				*f.dst = []string{name}
			}
			delete(host, f.name)
		}
	}
	if _, err := c.mask(); err != nil {
		return c, fmt.Errorf("%w: %v", errInvalid, err)
	}
	return c, nil
}
