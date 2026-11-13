package hostagent

import (
	"context"
	"io"
	"os/exec"
	"syscall"
)

// StartInMountNamespace launches the Go agent in its own mount namespace.
// The Go agent marks the root slave before it mounts any local volumes.
func StartInMountNamespace(ctx context.Context, binary string, console io.Writer, env ...string) (*Process, error) {
	cmd := exec.Command(binary)
	cmd.Env = append(cmd.Environ(), env...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Cloneflags: syscall.CLONE_NEWNS}
	return start(ctx, "/run", cmd, console)
}
