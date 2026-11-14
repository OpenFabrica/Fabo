package guestagent

import (
	"errors"
	"fmt"

	"golang.org/x/sys/unix"

	"github.com/OpenFabrica/Fabo/engine/internal/guestwire"
)

const defaultOpenFiles = 1 << 20

func applyProcessLimits(limits []guestwire.Ulimit) error {
	explicitOpenFiles := false
	for _, limit := range limits {
		res, soft, hard, err := limit.Values()
		if err != nil {
			return err
		}
		if err := unix.Setrlimit(res, &unix.Rlimit{Cur: soft, Max: hard}); err != nil {
			return fmt.Errorf("set ulimit %s: %w", limit.Name, err)
		}
		explicitOpenFiles = explicitOpenFiles || res == unix.RLIMIT_NOFILE
	}
	if explicitOpenFiles {
		return nil
	}
	err := unix.Setrlimit(unix.RLIMIT_NOFILE, &unix.Rlimit{Cur: defaultOpenFiles, Max: defaultOpenFiles})
	if errors.Is(err, unix.EPERM) || errors.Is(err, unix.EINVAL) {
		// A Linux service may be confined by its own inherited ceiling.
		var inherited unix.Rlimit
		if readErr := unix.Getrlimit(unix.RLIMIT_NOFILE, &inherited); readErr == nil && inherited.Max < defaultOpenFiles {
			inherited.Cur = inherited.Max
			err = unix.Setrlimit(unix.RLIMIT_NOFILE, &inherited)
		}
	}
	if err != nil {
		return fmt.Errorf("set default nofile ulimit: %w", err)
	}
	return nil
}
