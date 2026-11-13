package guestagent

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"

	"github.com/libracoder/kiri/engine/internal/guestwire"
)

type deviceNode struct {
	Target   string
	Mode     uint32
	Device   uint64
	UID, GID uint32
}

// A privileged container gets its own device nodes, never a mount of the
// guest's /dev. Links and the container's private pseudo-filesystems stay out.
func privilegedDeviceNodes() ([]deviceNode, error) {
	var nodes []deviceNode
	err := filepath.WalkDir("/dev", func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			if path == "/dev" {
				return err
			}
			return nil
		}
		switch path {
		case "/dev/pts", "/dev/shm", "/dev/mqueue", "/dev/.lxc", "/dev/.lxd-mounts", "/dev/.udev":
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		case "/dev/null", "/dev/zero", "/dev/full", "/dev/random", "/dev/urandom", "/dev/console", "/dev/ptmx", "/dev/tty":
			return nil
		}
		if entry.Type()&fs.ModeDevice != 0 {
			var info unix.Stat_t
			if unix.Lstat(path, &info) == nil && (info.Mode&unix.S_IFMT == unix.S_IFCHR || info.Mode&unix.S_IFMT == unix.S_IFBLK) {
				nodes = append(nodes, deviceNode{Target: path, Mode: info.Mode, Device: uint64(info.Rdev), UID: info.Uid, GID: info.Gid})
			}
		}
		return nil
	})
	return nodes, err
}

func makePrivilegedDevices(nodes []deviceNode) error {
	for _, node := range nodes {
		if err := os.MkdirAll(filepath.Dir(node.Target), 0755); err != nil {
			return err
		}
		if err := makeDeviceNode(node.Target, node); err != nil {
			return fmt.Errorf("create privileged device %s: %w", node.Target, err)
		}
	}
	return nil
}

func resolveDevices(options guestwire.DeviceOptions) ([]deviceNode, []guestwire.DeviceRule, error) {
	var nodes []deviceNode
	var rules []guestwire.DeviceRule
	for _, text := range options.Rules {
		rule, err := guestwire.ParseDeviceRule(text)
		if err != nil {
			return nil, nil, err
		}
		rules = append(rules, rule)
	}
	for _, mapping := range options.Mappings {
		mapping, err := mapping.Normalize()
		if err != nil {
			return nil, nil, err
		}
		access, _ := guestwire.DeviceAccess(mapping.CgroupPermissions)
		add := func(target string, info unix.Stat_t) bool {
			var kind uint32
			switch info.Mode & unix.S_IFMT {
			case unix.S_IFBLK:
				kind = unix.BPF_DEVCG_DEV_BLOCK
			case unix.S_IFCHR:
				kind = unix.BPF_DEVCG_DEV_CHAR
			default:
				return false
			}
			nodes = append(nodes, deviceNode{Target: target, Mode: info.Mode, Device: uint64(info.Rdev), UID: info.Uid, GID: info.Gid})
			rules = append(rules, guestwire.DeviceRule{
				Kind: kind, Major: int64(unix.Major(uint64(info.Rdev))), Minor: int64(unix.Minor(uint64(info.Rdev))), Access: access,
			})
			return true
		}
		err = collectDeviceNodes(mapping, add)
		if err != nil {
			return nil, nil, fmt.Errorf("error gathering device information while adding custom device %q: %w", mapping.PathOnHost, err)
		}
	}
	return nodes, rules, nil
}

func collectDeviceNodes(mapping guestwire.DeviceMapping, add func(string, unix.Stat_t) bool) error {
	var info unix.Stat_t
	if err := unix.Stat(mapping.PathOnHost, &info); err != nil {
		return err
	}
	if info.Mode&unix.S_IFMT != unix.S_IFDIR {
		if !add(mapping.PathInContainer, info) {
			return fmt.Errorf("source is not a device node or directory")
		}
		return nil
	}
	// Follow a symlink used as the source itself. WalkDir does not descend
	// into symlinked directories under it, avoiding loops in e.g. /dev/fd.
	root, err := filepath.EvalSymlinks(mapping.PathOnHost)
	if err != nil {
		return err
	}
	return filepath.WalkDir(root, func(source string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || entry.Type()&(fs.ModeDevice|fs.ModeSymlink) == 0 {
			return nil
		}
		var info unix.Stat_t
		if err := unix.Stat(source, &info); err != nil {
			if os.IsNotExist(err) && entry.Type()&fs.ModeSymlink != 0 {
				return nil
			}
			return err
		}
		relative, err := filepath.Rel(root, source)
		if err != nil {
			return err
		}
		add(filepath.Join(mapping.PathInContainer, relative), info)
		return nil
	})
}

func mountDeviceNodes(nodes []deviceNode) error {
	if len(nodes) == 0 {
		return nil
	}
	// /dev is private to this container. Clone nodes from its tmpfs so
	// paths outside /dev work on the nodev overlay and nodes are never
	// written into the persistent upper layer.
	dir, err := os.MkdirTemp("/dev", ".kiri-device-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	source := filepath.Join(dir, "node")
	for _, node := range nodes {
		if err := mountDeviceNode(source, node); err != nil {
			return fmt.Errorf("mount device at %q: %w", node.Target, err)
		}
	}
	return nil
}

func mountDeviceNode(source string, node deviceNode) error {
	if err := makeDeviceNode(source, node); err != nil {
		return err
	}
	defer os.Remove(source)
	tree, err := unix.OpenTree(unix.AT_FDCWD, source, unix.OPEN_TREE_CLONE|unix.OPEN_TREE_CLOEXEC)
	if err != nil {
		return err
	}
	defer unix.Close(tree)
	return attachMount(detachedMount{target: node.Target, fd: tree, copyFD: -1, device: true})
}

func makeDeviceNode(path string, node deviceNode) error {
	if err := unix.Mknod(path, node.Mode, int(node.Device)); err != nil {
		return err
	}
	if err := os.Chown(path, int(node.UID), int(node.GID)); err != nil {
		return err
	}
	return unix.Chmod(path, node.Mode&07777)
}
