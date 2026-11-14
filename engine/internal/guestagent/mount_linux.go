package guestagent

import (
	"fmt"
	"sort"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/OpenFabrica/FabriBay/engine/internal/guestwire"
)

type detachedMount struct {
	target string
	dir    bool
	device bool
	fd     int
	copyFD int
}

func openMounts(spec []string, vm bool) ([]detachedMount, error) {
	var mounts []detachedMount
	for _, item := range spec {
		if guestwire.IsTmpfsMount(item) {
			m, err := guestwire.DecodeTmpfsMount(item)
			if err != nil {
				closeMounts(mounts)
				return nil, err
			}
			mount, err := openTmpfs(m)
			if err != nil {
				closeMounts(mounts)
				return nil, fmt.Errorf("prepare tmpfs at %s: %w", m.Target, err)
			}
			mounts = append(mounts, mount)
			continue
		}
		m, err := guestwire.DecodeBindMount(item)
		if err != nil {
			closeMounts(mounts)
			return nil, err
		}
		bind, err := openBind(m, vm)
		if err != nil {
			closeMounts(mounts)
			return nil, fmt.Errorf("open bind source %s: %w", m.Source, err)
		}
		mounts = append(mounts, bind)
	}
	// Apply parent targets before any mounts below them.
	sort.SliceStable(mounts, func(i, j int) bool {
		return strings.Count(mounts[i].target, "/") < strings.Count(mounts[j].target, "/")
	})
	return mounts, nil
}

func openBind(m guestwire.BindMount, vm bool) (detachedMount, error) {
	// Pin the source first so its type and cloned tree describe the same object.
	source, err := openBindSource(m, vm)
	if err != nil {
		return detachedMount{}, err
	}
	defer unix.Close(source)
	var info unix.Stat_t
	if err := unix.Fstat(source, &info); err != nil {
		return detachedMount{}, err
	}
	isDir := info.Mode&unix.S_IFMT == unix.S_IFDIR
	if !isDir && info.Mode&unix.S_IFMT != unix.S_IFREG && info.Mode&unix.S_IFMT != unix.S_IFSOCK {
		return detachedMount{}, fmt.Errorf("source must be a regular file, socket or directory")
	}
	flags := uint(unix.OPEN_TREE_CLONE | unix.OPEN_TREE_CLOEXEC | unix.AT_EMPTY_PATH)
	attrs := uint(unix.AT_EMPTY_PATH)
	if isDir {
		flags |= unix.AT_RECURSIVE
		attrs |= unix.AT_RECURSIVE
	}
	tree, err := unix.OpenTree(source, "", flags)
	if err != nil {
		return detachedMount{}, err
	}
	mode := uint64(unix.MOUNT_ATTR_NOSUID | unix.MOUNT_ATTR_NODEV)
	if m.ReadOnly {
		mode |= unix.MOUNT_ATTR_RDONLY
	}
	if err := unix.MountSetattr(tree, "", attrs, &unix.MountAttr{Attr_set: mode, Propagation: unix.MS_PRIVATE}); err != nil {
		_ = unix.Close(tree)
		return detachedMount{}, err
	}
	copyFD := -1
	if m.CopyUp {
		if !isDir {
			_ = unix.Close(tree)
			return detachedMount{}, fmt.Errorf("volume copy-up requires a directory")
		}
		copyFD, err = unix.Openat(source, ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
		if err != nil {
			_ = unix.Close(tree)
			return detachedMount{}, err
		}
	}
	return detachedMount{target: m.Target, dir: isDir, fd: tree, copyFD: copyFD}, nil
}

func openBindSource(m guestwire.BindMount, vm bool) (int, error) {
	if !vm {
		return unix.Open(m.Source, unix.O_PATH|unix.O_CLOEXEC, 0)
	}
	rootPath := guestwire.HostSharePath
	relative, shared := strings.CutPrefix(m.Source, rootPath+"/")
	resolve := uint64(unix.RESOLVE_BENEATH | unix.RESOLVE_NO_MAGICLINKS)
	switch {
	case m.Create:
		name, ok := guestwire.VolumeNameAt(m.Source)
		if !ok {
			return -1, fmt.Errorf("invalid VM volume path %q", m.Source)
		}
		rootPath, relative = guestwire.VolumeRoot, name+"/_data"
		resolve |= unix.RESOLVE_NO_SYMLINKS
	case m.Source == rootPath:
		relative = "."
	case !shared:
		return unix.Open(m.Source, unix.O_PATH|unix.O_CLOEXEC, 0)
	}
	root, err := unix.Open(rootPath, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return -1, err
	}
	defer unix.Close(root)
	return unix.Openat2(root, relative, &unix.OpenHow{Flags: unix.O_PATH | unix.O_CLOEXEC, Resolve: resolve})
}

func closeMounts(mounts []detachedMount) {
	for _, b := range mounts {
		_ = unix.Close(b.fd)
		if b.copyFD >= 0 {
			_ = unix.Close(b.copyFD)
		}
	}
}

// Populate volumes while the overlay is still unobscured by user mounts.
func populateVolumes(rootPath string, mounts []detachedMount) error {
	root := -1
	defer func() {
		if root >= 0 {
			_ = unix.Close(root)
		}
	}()
	for _, mount := range mounts {
		if mount.copyFD < 0 {
			continue
		}
		if root < 0 {
			fd, err := unix.Open(rootPath, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
			if err != nil {
				return err
			}
			root = fd
		}
		source, err := unix.Openat2(root, mount.target, &unix.OpenHow{
			Flags: unix.O_PATH | unix.O_DIRECTORY | unix.O_CLOEXEC, Resolve: unix.RESOLVE_IN_ROOT | unix.RESOLVE_NO_MAGICLINKS,
		})
		if err == unix.ENOENT || err == unix.ENOTDIR {
			continue
		}
		if err == nil {
			err = copyUpVolume(source, mount.copyFD)
			_ = unix.Close(source)
		}
		if err != nil {
			return fmt.Errorf("populate volume at %s: %w", mount.target, err)
		}
	}
	return nil
}

func attachMounts(mounts []detachedMount) error {
	for _, b := range mounts {
		if err := attachMount(b); err != nil {
			return fmt.Errorf("mount %s: %w", b.target, err)
		}
	}
	return nil
}

func attachMount(b detachedMount) error {
	current, err := unix.Open("/", unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer func() { _ = unix.Close(current) }()
	root, err := unix.Open("/", unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(root)
	parts := strings.Split(strings.TrimPrefix(b.target, "/"), "/")
	for i, part := range parts {
		dir := i < len(parts)-1 || b.dir
		next, err := openTarget(root, current, part, strings.Join(parts[:i+1], "/"), dir, b.device)
		if err != nil {
			return err
		}
		_ = unix.Close(current)
		current = next
	}
	return unix.MoveMount(b.fd, "", current, "", unix.MOVE_MOUNT_F_EMPTY_PATH|unix.MOVE_MOUNT_T_EMPTY_PATH)
}

func openTarget(root, parent int, name, path string, dir, device bool) (int, error) {
	flags := unix.O_PATH | unix.O_CLOEXEC
	if dir {
		flags |= unix.O_DIRECTORY
	}
	open := func() (int, error) {
		return unix.Openat2(root, path, &unix.OpenHow{
			Flags: uint64(flags), Resolve: unix.RESOLVE_IN_ROOT | unix.RESOLVE_NO_MAGICLINKS,
		})
	}
	fd, err := open()
	if err == unix.ENOENT {
		if dir {
			if err := unix.Mkdirat(parent, name, 0755); err != nil && err != unix.EEXIST {
				return -1, err
			}
		} else {
			file, err := unix.Openat(parent, name, unix.O_CREAT|unix.O_EXCL|unix.O_WRONLY|unix.O_CLOEXEC, 0644)
			if err != nil && err != unix.EEXIST {
				return -1, err
			}
			if err == nil {
				_ = unix.Close(file)
			}
		}
		fd, err = open()
	}
	if err != nil {
		return -1, err
	}
	if !dir {
		var info unix.Stat_t
		if err := unix.Fstat(fd, &info); err != nil {
			_ = unix.Close(fd)
			return -1, err
		}
		kind := info.Mode & unix.S_IFMT
		if kind != unix.S_IFREG && !(device && (kind == unix.S_IFCHR || kind == unix.S_IFBLK)) && !(!device && kind == unix.S_IFSOCK) {
			_ = unix.Close(fd)
			return -1, fmt.Errorf("invalid mount target type for %q", name)
		}
	}
	return fd, nil
}
