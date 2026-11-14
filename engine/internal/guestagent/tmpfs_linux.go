package guestagent

import (
	"strings"

	"golang.org/x/sys/unix"

	"github.com/OpenFabrica/Fabo/engine/internal/guestwire"
)

// A detached tmpfs goes through the same ordered, descriptor-relative
// attachment as binds. Its contents are private to this run.
func openTmpfs(m guestwire.TmpfsMount) (detachedMount, error) {
	fs, err := unix.Fsopen("tmpfs", unix.FSOPEN_CLOEXEC)
	if err != nil {
		return detachedMount{}, err
	}
	defer unix.Close(fs)
	if err := unix.FsconfigSetString(fs, "source", "tmpfs"); err != nil {
		return detachedMount{}, err
	}
	flags := unix.MOUNT_ATTR_NOEXEC | unix.MOUNT_ATTR_NOSUID | unix.MOUNT_ATTR_NODEV
	for _, option := range m.Options {
		switch option {
		case "ro":
			flags |= unix.MOUNT_ATTR_RDONLY
		case "rw":
			flags &^= unix.MOUNT_ATTR_RDONLY
		case "exec":
			flags &^= unix.MOUNT_ATTR_NOEXEC
		case "noexec":
			flags |= unix.MOUNT_ATTR_NOEXEC
		case "suid":
			flags &^= unix.MOUNT_ATTR_NOSUID
		case "nosuid":
			flags |= unix.MOUNT_ATTR_NOSUID
		case "dev":
			flags &^= unix.MOUNT_ATTR_NODEV
		case "nodev":
			flags |= unix.MOUNT_ATTR_NODEV
		case "atime":
			flags &^= unix.MOUNT_ATTR_NOATIME
		case "noatime":
			flags |= unix.MOUNT_ATTR_NOATIME
		default:
			key, value, _ := strings.Cut(option, "=")
			if err := unix.FsconfigSetString(fs, key, value); err != nil {
				return detachedMount{}, err
			}
		}
	}
	if err := unix.FsconfigCreate(fs); err != nil {
		return detachedMount{}, err
	}
	mount, err := unix.Fsmount(fs, unix.FSMOUNT_CLOEXEC, flags)
	if err != nil {
		return detachedMount{}, err
	}
	return detachedMount{target: m.Target, dir: true, fd: mount, copyFD: -1}, nil
}
