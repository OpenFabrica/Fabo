//go:build linux || darwin

package localengine

import (
	"encoding/json"
	"fmt"

	"github.com/OpenFabrica/Fabo/engine/internal/guestwire"
)

func parseSecurityOptions(raw json.RawMessage) ([]string, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var options []string
	if err := json.Unmarshal(raw, &options); err != nil {
		var option string
		if err := json.Unmarshal(raw, &option); err != nil {
			return nil, fmt.Errorf("%w: invalid SecurityOpt", errInvalid)
		}
		options = []string{option}
	}
	return options, nil
}

func securityFlags(options []string) (uint32, error) {
	var flags uint32
	for _, option := range options {
		switch option {
		case "seccomp=unconfined", "seccomp:unconfined":
			flags |= guestwire.RunNoSeccomp
		case "no-new-privileges", "no-new-privileges=true", "no-new-privileges:true":
			flags |= guestwire.RunNoNewPrivs
		case "no-new-privileges=false", "no-new-privileges:false":
			// The Linux default is to allow privileges on exec.
		default:
			return 0, fmt.Errorf("%w: unsupported security option %q", errInvalid, option)
		}
	}
	return flags, nil
}
