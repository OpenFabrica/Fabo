//go:build linux || darwin

package localengine

import (
	"errors"
	"os"
	"path/filepath"

	"github.com/OpenFabrica/FabriBay/engine/internal/guestwire"
)

func engineSocketAlias(source string) bool {
	switch filepath.Clean(source) {
	case "/var/run/docker.sock", "/run/docker.sock", "/private/var/run/docker.sock":
		return true
	}
	return false
}

func (e *Engine) isEngineSocket(source string) bool {
	if e.engineSocket == "" {
		return false
	}
	return engineSocketAlias(source) || filepath.Clean(source) == filepath.Clean(e.engineSocket) ||
		e.remoteGuest && source == guestwire.EngineSocketPath
}

func (e *Engine) runtimeSocketMounts(mounts []string) ([]string, error) {
	mapped := make([]string, 0, len(mounts))
	for _, value := range mounts {
		if guestwire.IsTmpfsMount(value) {
			mapped = append(mapped, value)
			continue
		}
		m, err := guestwire.DecodeBindMount(value)
		if err != nil {
			return nil, err
		}
		if e.isEngineSocket(m.Source) {
			if e.remoteGuest {
				m.Source = guestwire.EngineSocketPath
			} else if engineSocketAlias(m.Source) {
				if _, err := os.Stat(m.Source); errors.Is(err, os.ErrNotExist) {
					m.Source = e.engineSocket
				}
			}
		}
		value, err = m.Encode()
		if err != nil {
			return nil, err
		}
		mapped = append(mapped, value)
	}
	return mapped, nil
}
