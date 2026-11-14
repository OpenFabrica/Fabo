package guestagent

import (
	"errors"
	"fmt"
	"os"
	"sync"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// retiredVeths deletes the host ends of exited containers' veths off the
// exit path. Deleting a veth waits for an RCU barrier in the kernel, about
// 20 ms even with expedited grace periods, which used to delay every bridge
// container's Exited event by that much.
var retiredVeths vethRetirer

// vethRetirer counts the deletions still running in the background, so
// shutdown can wait for them.
type vethRetirer struct {
	mu      sync.Mutex
	idle    *sync.Cond
	pending int
	seq     uint64
}

// retire takes link out of service at once and deletes it in the background.
// Leaving the bridge frees the container's address for reuse, and the
// tombstone name frees "kv<id>" for the same container's next start.
// Everything here addresses the link by index, so the background deletion
// can never hit a newer veth that took the old name.
func (v *vethRetirer) retire(link netlink.Link) {
	if link == nil {
		return
	}
	if err := detachVeth(link, v.tombstone()); err != nil {
		if vethGone(err) {
			return
		}
		// Without the rename the next start could collide with this name:
		// delete it now, as before.
		fmt.Fprintln(os.Stderr, "detach container veth:", err)
		if err := deleteVeth(link); err != nil {
			fmt.Fprintln(os.Stderr, "remove container veth:", err)
		}
		return
	}
	v.mu.Lock()
	v.pending++
	v.mu.Unlock()
	go func() {
		if err := deleteVeth(link); err != nil {
			fmt.Fprintln(os.Stderr, "remove container veth:", err)
		}
		v.mu.Lock()
		v.pending--
		if v.pending == 0 {
			v.cond().Broadcast()
		}
		v.mu.Unlock()
	}()
}

// wait returns once every retired veth has been deleted.
func (v *vethRetirer) wait() {
	v.mu.Lock()
	defer v.mu.Unlock()
	for v.pending > 0 {
		v.cond().Wait()
	}
}

// cond returns the condition signalled when no deletion is pending. The
// caller holds v.mu.
func (v *vethRetirer) cond() *sync.Cond {
	if v.idle == nil {
		v.idle = sync.NewCond(&v.mu)
	}
	return v.idle
}

// tombstone returns a fresh 15-character interface name for a retired veth.
// The process ID keeps names apart when several Linux-host engines share one
// network namespace; inside a VM the agent is always PID 1.
func (v *vethRetirer) tombstone() string {
	v.mu.Lock()
	v.seq++
	seq := v.seq
	v.mu.Unlock()
	return tombstoneName(os.Getpid(), seq)
}

// tombstoneName spells a retired veth's name: "kr", 6 hex digits of the
// process ID and 7 of the sequence, the most IFNAMSIZ allows.
func tombstoneName(pid int, seq uint64) string {
	return fmt.Sprintf("kr%06x%07x", pid&0xffffff, seq&0xfffffff)
}

// detachVeth removes link from its bridge, stops it and renames it, each a
// fraction of a millisecond. A link must be down to be renamed.
func detachVeth(link netlink.Link, name string) error {
	if err := netlink.LinkSetNoMaster(link); err != nil {
		return err
	}
	if err := netlink.LinkSetDown(link); err != nil {
		return err
	}
	return netlink.LinkSetName(link, name)
}

// vethGone reports an error meaning the link no longer exists, as when the
// kernel already removed the pair with the container's network namespace.
func vethGone(err error) bool {
	var missing netlink.LinkNotFoundError
	return errors.Is(err, unix.ENODEV) || errors.As(err, &missing)
}
