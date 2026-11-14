package guestagent

import (
	"bytes"
	"fmt"
	"io"
	"strings"
)

// setupReady is the byte a container's child writes on its status pipe once
// it has entered its own root and namespaces, just before resolving and
// exec'ing the command. Anything after it is a command failure.
const setupReady = 0x06

// readSetup follows the status pipe of a container reported started early.
// It closes setupDone as soon as the outcome of the setup is known, then
// calls settled if the setup succeeded, and returns any failure message. A
// pipe that closes without setupReady, as when the child dies first, is a
// failed setup: the child may never have left the agent's root.
func (child *childProcess) readSetup(settled func()) string {
	defer func() { _ = child.setup.Close() }()
	var first [1]byte
	n, _ := io.ReadFull(child.setup, first[:])
	child.setupFailed = n == 0 || first[0] != setupReady
	close(child.setupDone)
	if !child.setupFailed {
		settled()
	}
	rest, _ := io.ReadAll(io.LimitReader(child.setup, 4096))
	if n == 1 && first[0] != setupReady {
		rest = append(first[:], rest...)
	}
	return strings.TrimSpace(string(bytes.TrimPrefix(rest, []byte{setupReady})))
}

// settled reports whether the child has finished entering its root and
// namespaces, and whether that succeeded. A child that waited for its exec
// before being reported started is settled from the start.
func (child *childProcess) settled() (done, ok bool) {
	if child.setupDone == nil {
		return true, true
	}
	select {
	case <-child.setupDone:
		return true, !child.setupFailed
	default:
		return false, false
	}
}

// requireSettled fails, without waiting, unless the child has entered its
// own root and namespaces. Every request on the serial control channel that
// reads them uses it: a setup may take long, copying a whole image directory
// into a new volume, and anything in the container can stall it, as a
// process sharing its PID namespace stopping it, so waiting there could hold
// the very request that resumes it. The host waits for the Settled event
// before sending exec or a network join, so this only guards against a
// request that raced a restart.
func (child *childProcess) requireSettled(id string) error {
	switch done, ok := child.settled(); {
	case !done:
		return fmt.Errorf("container %s is still starting", id)
	case !ok:
		return fmt.Errorf("container %s failed to start", id)
	}
	return nil
}

// lockSettled locks r.mu and returns the child running as id, or nil, once
// that child has entered its own root and namespaces. Callers that open
// /proc/<pid>/root or the child's namespaces need this: before then they
// would capture the agent's. It waits without the lock, as the setup may
// copy a whole image directory into a new volume, and looks the child up
// again afterwards, since the container may have been restarted meanwhile.
// On error it returns with r.mu unlocked.
//
// Only archive copies use it: each runs on its own connection, so waiting
// holds nothing else. The serial control channel must not (requireSettled).
func (r *Runtime) lockSettled(id string) (*childProcess, error) {
	for {
		r.mu.Lock()
		child := r.children[id]
		if child == nil {
			return nil, nil
		}
		done, ok := child.settled()
		if done && ok {
			return child, nil
		}
		r.mu.Unlock()
		if done {
			return nil, fmt.Errorf("container %s failed to start", id)
		}
		<-child.setupDone
	}
}
