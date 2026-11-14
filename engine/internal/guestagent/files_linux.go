package guestagent

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"slices"
	"strings"
	"syscall"
	"unicode/utf8"

	"golang.org/x/sys/unix"

	"github.com/OpenFabrica/Fabo/engine/internal/guestwire"
)

// runningRoot pins the running process's mounted root while holding the
// runtime lock. The caller closes the returned descriptor.
func (r *Runtime) runningRoot(id string) (*os.File, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	child := r.children[id]
	if child == nil || child.isExec {
		return nil, fmt.Errorf("container %s is not running", id)
	}
	return openProcessRoot(child)
}

func openProcessRoot(child *childProcess) (*os.File, error) {
	fd, err := unix.Open(fmt.Sprintf("/proc/%d/root", child.Process.Pid), unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	if err := child.Process.Signal(syscall.Signal(0)); err != nil {
		_ = unix.Close(fd)
		return nil, err
	}
	return os.NewFile(uintptr(fd), "container-root"), nil
}

func (r *Runtime) fileQuery(frame guestwire.Frame) (guestwire.Frame, error) {
	q, err := guestwire.DecodeFileQuery(frame)
	if err != nil {
		return guestwire.Frame{}, err
	}
	root, err := r.runningRoot(q.ID)
	if err != nil {
		return guestwire.Frame{}, err
	}
	defer root.Close()
	switch frame.Kind {
	case guestwire.FileStatRequest:
		entry, err := fileViewStat(root, q.Path)
		if err != nil {
			return guestwire.Frame{}, err
		}
		return entry.Frame()
	case guestwire.FileListRequest:
		entries, err := fileViewList(root, q.Path)
		if err != nil {
			return guestwire.Frame{}, err
		}
		return guestwire.FileListFrame(entries)
	default:
		data, err := fileViewRead(root, q)
		return guestwire.FileDataFrame(data), err
	}
}

// Absolute symlinks and parent traversal resolve inside the container.
// Kernel magic links such as /proc/*/root cannot be followed.
func openViewPath(root *os.File, name string, flags uint64) (int, error) {
	return unix.Openat2(int(root.Fd()), name, &unix.OpenHow{
		Flags: flags | unix.O_CLOEXEC, Resolve: unix.RESOLVE_IN_ROOT | unix.RESOLVE_NO_MAGICLINKS,
	})
}

func viewMetadata(name string, st unix.Stat_t) guestwire.FileInfo {
	return guestwire.FileInfo{
		Name: name, Mode: st.Mode, Size: uint64(max(st.Size, 0)),
		Mtime: st.Mtim.Sec, MtimeNS: uint32(st.Mtim.Nsec), Inode: st.Ino,
	}
}

func fileViewStat(root *os.File, name string) (guestwire.FileInfo, error) {
	fd, err := openViewPath(root, name, unix.O_PATH)
	if err != nil {
		// A dangling symlink can still be listed as a symlink.
		fd, err = openViewPath(root, name, unix.O_PATH|unix.O_NOFOLLOW)
	}
	if err != nil {
		return guestwire.FileInfo{}, err
	}
	defer unix.Close(fd)
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return guestwire.FileInfo{}, err
	}
	return viewMetadata(path.Base(name), st), nil
}

func fileViewList(root *os.File, name string) ([]guestwire.FileInfo, error) {
	fd, err := openViewPath(root, name, unix.O_RDONLY|unix.O_DIRECTORY)
	if err != nil {
		return nil, err
	}
	dir := os.NewFile(uintptr(fd), name)
	defer dir.Close()
	names, err := dir.Readdirnames(guestwire.MaxFileEntries)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	slices.Sort(names)
	entries := make([]guestwire.FileInfo, 0, len(names))
	for _, child := range names {
		if !utf8.ValidString(child) {
			continue
		}
		var st unix.Stat_t
		if err := unix.Fstatat(fd, child, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			continue
		}
		entry := viewMetadata(child, st)
		if st.Mode&unix.S_IFMT == unix.S_IFLNK {
			childPath := strings.TrimRight(name, "/") + "/" + child
			if target, err := fileViewStat(root, childPath); err == nil {
				entry = target
			}
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

func fileViewRead(root *os.File, q guestwire.FileQuery) ([]byte, error) {
	fd, err := openViewPath(root, q.Path, unix.O_PATH)
	if err != nil {
		return nil, err
	}
	defer unix.Close(fd)
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return nil, err
	}
	if st.Mode&unix.S_IFMT == unix.S_IFDIR {
		return nil, unix.EISDIR
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG {
		return nil, unix.EINVAL
	}
	file, err := os.Open(fmt.Sprintf("/proc/self/fd/%d", fd))
	if err != nil {
		return nil, err
	}
	defer file.Close()
	buf := make([]byte, q.Count)
	n, err := file.ReadAt(buf, int64(q.Offset))
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return buf[:n], nil
}
