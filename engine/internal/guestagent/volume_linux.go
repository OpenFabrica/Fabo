package guestagent

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/OpenFabrica/Fabo/engine/internal/guestwire"
)

// ensureVolumeMounts prepares volume roots before the container clones the
// agent's mount namespace. A driver mount is shared by every user of the volume.
func (r *Runtime) ensureVolumeMounts(mounts []string) error {
	for _, spec := range mounts {
		if guestwire.IsTmpfsMount(spec) {
			continue
		}
		mount, err := guestwire.DecodeBindMount(spec)
		if err != nil {
			return err
		}
		if !mount.Create && mount.Driver == nil {
			continue
		}
		name := filepath.Base(filepath.Dir(mount.Source))
		relative := filepath.Join(name, "_data")
		if r.volumeRoot == "" || !guestwire.ValidVolumeName(name) || mount.Source != filepath.Join(r.volumeRoot, relative) {
			return fmt.Errorf("invalid local volume path %q", mount.Source)
		}
		if err := r.withVolumeRoot(func(root *os.Root) error { return root.MkdirAll(relative, 0755) }); err != nil {
			return fmt.Errorf("prepare volume %q: %w", name, err)
		}
		if mount.Driver != nil && r.driverMounts[name] == "" {
			if err := r.mountDriverVolume(relative, mount); err != nil {
				return fmt.Errorf("mount local volume %q: %w", name, err)
			}
			r.driverMounts[name] = mount.Source
		}
	}
	return nil
}

func (r *Runtime) deleteVolume(name string) error {
	if r.volumeRoot == "" || !guestwire.ValidVolumeName(name) {
		return fmt.Errorf("invalid or unavailable volume %q", name)
	}
	if err := r.releaseDriverVolume(name); err != nil {
		return err
	}
	if !r.vm {
		// The native host owns the volume record and its unmounted directory.
		return nil
	}
	return r.withVolumeRoot(func(root *os.Root) error {
		return root.RemoveAll(name)
	})
}

func (r *Runtime) withVolumeRoot(use func(*os.Root) error) error {
	root, err := os.OpenRoot(r.volumeRoot)
	if err != nil {
		return err
	}
	defer root.Close()
	return use(root)
}

func (r *Runtime) mountDriverVolume(relative string, m guestwire.BindMount) error {
	root, err := unix.Open(r.volumeRoot, unix.O_PATH|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(root)
	target, err := unix.Openat2(root, relative, &unix.OpenHow{
		Flags: unix.O_PATH | unix.O_DIRECTORY | unix.O_CLOEXEC, Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS,
	})
	if err != nil {
		return err
	}
	defer unix.Close(target)
	targetFD := fmt.Sprintf("/proc/self/fd/%d", target)
	v := m.Driver
	flags, data := localVolumeFlags(v.Options)
	if flags&unix.MS_BIND == 0 {
		return unix.Mount(v.Device, targetFD, v.Type, flags, data)
	}
	fd, err := openBindSource(guestwire.BindMount{Source: v.Device}, r.vm)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	clone := uint(unix.OPEN_TREE_CLONE | unix.OPEN_TREE_CLOEXEC | unix.AT_EMPTY_PATH)
	mode := uint(unix.AT_EMPTY_PATH)
	if flags&unix.MS_REC != 0 {
		clone |= unix.AT_RECURSIVE
		mode |= unix.AT_RECURSIVE
	}
	tree, err := unix.OpenTree(fd, "", clone)
	if err != nil {
		return err
	}
	defer unix.Close(tree)
	attrs := uint64(0)
	if flags&unix.MS_RDONLY != 0 {
		attrs |= unix.MOUNT_ATTR_RDONLY
	}
	if flags&unix.MS_NOEXEC != 0 {
		attrs |= unix.MOUNT_ATTR_NOEXEC
	}
	if flags&unix.MS_NOSUID != 0 {
		attrs |= unix.MOUNT_ATTR_NOSUID
	}
	if flags&unix.MS_NODEV != 0 {
		attrs |= unix.MOUNT_ATTR_NODEV
	}
	if attrs != 0 {
		if err := unix.MountSetattr(tree, "", mode, &unix.MountAttr{Attr_set: attrs}); err != nil {
			return err
		}
	}
	return unix.MoveMount(tree, "", target, "", unix.MOVE_MOUNT_F_EMPTY_PATH|unix.MOVE_MOUNT_T_EMPTY_PATH)
}

func (r *Runtime) releaseDriverVolume(name string) error {
	if target := r.driverMounts[name]; target != "" {
		if err := unix.Unmount(target, unix.MNT_DETACH); err != nil {
			return err
		}
		delete(r.driverMounts, name)
	}
	return nil
}

var localVolumeFlagOptions = map[string]struct{ set, clear uintptr }{
	"defaults": {}, "ro": {set: unix.MS_RDONLY}, "rw": {clear: unix.MS_RDONLY},
	"suid": {clear: unix.MS_NOSUID}, "nosuid": {set: unix.MS_NOSUID},
	"dev": {clear: unix.MS_NODEV}, "nodev": {set: unix.MS_NODEV},
	"exec": {clear: unix.MS_NOEXEC}, "noexec": {set: unix.MS_NOEXEC},
	"sync": {set: unix.MS_SYNCHRONOUS}, "async": {clear: unix.MS_SYNCHRONOUS},
	"remount": {set: unix.MS_REMOUNT}, "mand": {set: unix.MS_MANDLOCK}, "nomand": {clear: unix.MS_MANDLOCK},
	"dirsync": {set: unix.MS_DIRSYNC}, "atime": {clear: unix.MS_NOATIME}, "noatime": {set: unix.MS_NOATIME},
	"diratime": {clear: unix.MS_NODIRATIME}, "nodiratime": {set: unix.MS_NODIRATIME},
	"bind": {set: unix.MS_BIND}, "rbind": {set: unix.MS_BIND | unix.MS_REC},
	"relatime": {set: unix.MS_RELATIME}, "norelatime": {clear: unix.MS_RELATIME},
	"strictatime": {set: unix.MS_STRICTATIME}, "nostrictatime": {clear: unix.MS_STRICTATIME},
}

func localVolumeFlags(options string) (uintptr, string) {
	var flags uintptr
	var data []string
	for option := range strings.SplitSeq(options, ",") {
		if v, ok := localVolumeFlagOptions[option]; ok {
			flags = flags&^v.clear | v.set
			continue
		}
		switch option {
		case "", "private", "rprivate", "shared", "rshared", "slave", "rslave", "unbindable", "runbindable":
		default:
			data = append(data, option)
		}
	}
	return flags, strings.Join(data, ",")
}
