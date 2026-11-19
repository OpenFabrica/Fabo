package main

import (
	"context"
	"net"
	"os"
	"os/exec"
	"strings"
	"syscall"

	"github.com/Microsoft/go-winio"
)

// enginePipe is the named pipe the engine listens on.
func enginePipe() string {
	if pipe := os.Getenv("FABO_PIPE"); pipe != "" {
		return pipe
	}
	return `\\.\pipe\fabo`
}

func dialEngine(ctx context.Context) (net.Conn, error) {
	return winio.DialPipeContext(ctx, enginePipe())
}

// engineHost is the engine's address as DOCKER_HOST spells it, for the
// docker tools the window runs.
func engineHost() string {
	return "npipe://" + strings.ReplaceAll(enginePipe(), `\`, "/")
}

// dialHost connects to fabo-win's control pipe.
func dialHost(ctx context.Context) (net.Conn, error) {
	return winio.DialPipeContext(ctx, `\\.\pipe\fabo-win`)
}

// singleInstance: fabo-win starts the window each time Fabo is opened, and
// the running one comes forward instead of a second.
const singleInstance = true

// hideConsole keeps a console program the window runs (bin\fabo.exe,
// docker.exe, a credential helper) from flashing up a console window.
func hideConsole(c *exec.Cmd) {
	c.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000} // CREATE_NO_WINDOW
}

// endGroup leaves the command as it is: on Windows the context's kill
// ends docker.exe, and WaitDelay stops waiting on a plugin that outlives it.
func endGroup(*exec.Cmd) {}
