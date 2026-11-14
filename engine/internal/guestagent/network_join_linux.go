package guestagent

import (
	"fmt"
	"os"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"

	"github.com/OpenFabrica/Fabo/engine/internal/guestwire"
)

func joinNetworkTarget(spec guestwire.RunSpec) (string, error) {
	n := spec.Network
	for _, option := range n.Options {
		id, ok := strings.CutPrefix(option, guestwire.NetworkJoinPrefix)
		if !ok {
			continue
		}
		if !validContainerID(id) || id == spec.ID || len(n.Options) != 1 || n.Bridge != "" ||
			n.IP != [4]byte{} || n.Gateway != [4]byte{} || n.Prefix != 0 ||
			len(n.ExtraHosts)+len(n.DNSNames)+len(spec.AdditionalNetworks) != 0 {
			return "", fmt.Errorf("invalid shared container network")
		}
		return id, nil
	}
	return "", nil
}

// openSharedNetwork pins the target's namespace until the child enters it.
func openSharedNetwork(owner *childProcess) (*os.File, error) {
	proc := fmt.Sprintf("/proc/%d/ns/net", owner.Process.Pid)
	ns, err := os.Open(proc)
	if err != nil {
		return nil, fmt.Errorf("open target network namespace: %w", err)
	}
	if err := owner.Process.Signal(syscall.Signal(0)); err != nil {
		_ = ns.Close()
		return nil, fmt.Errorf("check network container process: %w", err)
	}
	return ns, nil
}

func enterSharedNetwork(flags uint32) error {
	// fd 3 is the status pipe; interactive runs also have a transport at fd 4.
	fd := 4
	if flags&(guestwire.RunTTY|guestwire.RunStdin) != 0 {
		fd++
	}
	err := unix.Setns(fd, unix.CLONE_NEWNET)
	_ = unix.Close(fd)
	if err != nil {
		return fmt.Errorf("join container network namespace: %w", err)
	}
	return nil
}
