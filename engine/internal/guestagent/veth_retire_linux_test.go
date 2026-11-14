package guestagent

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// Tombstone names fit IFNAMSIZ, keep the "kr" prefix and never repeat.
func TestTombstoneName(t *testing.T) {
	seen := map[string]bool{}
	for _, c := range []struct {
		pid int
		seq uint64
	}{{1, 1}, {1, 2}, {0xffffff, 0xfffffff}, {4242, 7}} {
		name := tombstoneName(c.pid, c.seq)
		if len(name) != 15 || !strings.HasPrefix(name, "kr") || seen[name] {
			t.Fatalf("tombstoneName(%d, %d) = %q", c.pid, c.seq, name)
		}
		seen[name] = true
	}
}

// testVeth creates a bridge and a veth attached to it, as attach does, or
// skips without the privileges to change links.
func testVeth(t *testing.T, name string) (*netlink.Bridge, netlink.Link) {
	t.Helper()
	bridgeAttrs := netlink.NewLinkAttrs()
	bridgeAttrs.Name = fmt.Sprintf("fbt%d", os.Getpid()%100000)
	bridge := &netlink.Bridge{LinkAttrs: bridgeAttrs}
	if err := netlink.LinkAdd(bridge); err != nil {
		if errors.Is(err, unix.EPERM) || errors.Is(err, unix.EACCES) {
			t.Skip("changing network links needs CAP_NET_ADMIN")
		}
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = netlink.LinkDel(bridge) })
	return bridge, addTestVeth(t, bridge, name, name+"p")
}

// addTestVeth creates a veth named name on bridge and brings it up. In a
// container the peer lives in a fresh namespace; here both ends share the
// test's, so each veth needs its own peer name.
func addTestVeth(t *testing.T, bridge *netlink.Bridge, name, peer string) netlink.Link {
	t.Helper()
	attrs := netlink.NewLinkAttrs()
	attrs.Name = name
	veth := &netlink.Veth{LinkAttrs: attrs, PeerName: peer}
	if err := netlink.LinkAdd(veth); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = deleteVeth(veth) })
	if err := netlink.LinkSetMaster(veth, bridge); err != nil {
		t.Fatal(err)
	}
	if err := netlink.LinkSetUp(veth); err != nil {
		t.Fatal(err)
	}
	return veth
}

// A retired veth leaves its bridge and its name at once, so the same
// container can start again immediately, and is gone after wait.
func TestRetireVethFreesNameAndBridge(t *testing.T) {
	name := fmt.Sprintf("kvt%d", os.Getpid()%100000)
	bridge, veth := testVeth(t, name)
	index := veth.Attrs().Index

	retireVeth(veth)

	if _, err := netlink.LinkByName(name); !vethGone(err) {
		t.Fatalf("old name still in use: %v", err)
	}
	// The background deletion may already have finished; if not, the
	// retired link must be off the bridge, down and renamed.
	if old, err := netlink.LinkByIndex(index); err == nil {
		attrs := old.Attrs()
		if attrs.MasterIndex != 0 || attrs.Flags&unix.IFF_UP != 0 || !strings.HasPrefix(attrs.Name, "kr") {
			t.Fatalf("retired veth still in service: master %d, flags %v, name %q", attrs.MasterIndex, attrs.Flags, attrs.Name)
		}
	} else if !vethGone(err) {
		t.Fatal(err)
	}
	again := addTestVeth(t, bridge, name, name+"q")
	if again.Attrs().Index == index {
		t.Fatal("new veth reused the retired index")
	}

	retiredVeths.wait()
	if _, err := netlink.LinkByIndex(index); !vethGone(err) {
		t.Fatalf("retired veth not deleted: %v", err)
	}
	if _, err := netlink.LinkByName(name); err != nil {
		t.Fatalf("background deletion removed the new veth: %v", err)
	}
}

// Retiring a veth the kernel already removed, with its namespace, is a
// no-op and leaves nothing pending.
func TestRetireVethAlreadyGone(t *testing.T) {
	name := fmt.Sprintf("kvg%d", os.Getpid()%100000)
	_, veth := testVeth(t, name)
	if err := netlink.LinkDel(veth); err != nil {
		t.Fatal(err)
	}
	retireVeth(veth)
	retiredVeths.mu.Lock()
	pending := retiredVeths.pending
	retiredVeths.mu.Unlock()
	if pending != 0 {
		t.Fatalf("pending deletions = %d", pending)
	}
	retiredVeths.wait()
}
