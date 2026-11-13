package guestagent

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"

	"github.com/libracoder/kiri/engine/internal/guestwire"
	"golang.org/x/sys/unix"
)

type copyConfig struct {
	Spec    guestwire.CopySpec
	Overlay *processConfig
}

// ServeCopy runs one transfer through an isolated child. file provides a
// connection descriptor that the child owns for the remainder of the job.
func (r *Runtime) ServeCopy(ctx context.Context, conn io.ReadWriter, file func() (*os.File, error)) {
	frame, err := guestwire.ReadFrame(conn, nil)
	if err != nil {
		return
	}
	spec, err := guestwire.DecodeCopy(frame)
	if err != nil {
		copyReply(conn, err)
		return
	}
	if !validContainerID(spec.ID) || !filepath.IsAbs(spec.Path) ||
		(spec.Op < guestwire.CopyIn || spec.Op > guestwire.CopyCommit) ||
		(spec.Op == guestwire.CopyIn && spec.Flags&guestwire.CopyChunked == 0) {
		copyReply(conn, fmt.Errorf("invalid container copy request"))
		return
	}

	cfg, files, err := r.copyRoot(spec)
	if err != nil {
		copySetupFailed(conn, spec, err)
		return
	}
	for _, f := range files {
		defer f.Close()
	}
	payload, err := json.Marshal(cfg)
	if err != nil {
		copySetupFailed(conn, spec, err)
		return
	}
	socket, err := file()
	if err != nil {
		copySetupFailed(conn, spec, err)
		return
	}
	defer socket.Close()
	cmd := exec.CommandContext(ctx, "/proc/self/exe", "__kiri-copy")
	cmd.Stdin = bytes.NewReader(payload)
	cmd.Stderr = os.Stderr
	cmd.ExtraFiles = append([]*os.File{socket}, files...)
	if cfg.Overlay != nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{Cloneflags: unix.CLONE_NEWNS}
	}
	if err := cmd.Start(); err != nil {
		copySetupFailed(conn, spec, err)
		return
	}
	_ = cmd.Wait()
}

func (r *Runtime) copyRoot(spec guestwire.CopySpec) (copyConfig, []*os.File, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cfg := copyConfig{Spec: spec}
	if child := r.children[spec.ID]; child != nil && !child.isExec && spec.Op != guestwire.CopyDiff && spec.Op != guestwire.CopyCommit {
		proc := fmt.Sprintf("/proc/%d", child.Process.Pid)
		root, err := openProcessRoot(child)
		if err != nil {
			return cfg, nil, err
		}
		if spec.Op == guestwire.CopyExport {
			ns, err := os.Open(proc + "/ns/mnt")
			if err != nil {
				root.Close()
				return cfg, nil, err
			}
			return cfg, []*os.File{root, ns}, nil
		}
		return cfg, []*os.File{root}, nil
	}
	overlay, err := r.prepareRoot(guestwire.RunSpec{ID: spec.ID, Layers: spec.Layers})
	if err != nil {
		return cfg, nil, err
	}
	if spec.Op == guestwire.CopyDiff && len(overlay.Lower) == 1 {
		// OverlayFS requires two lower directories when there is no upper.
		empty := filepath.Join(r.dir, ".diff-empty-layer")
		if err := os.MkdirAll(empty, 0755); err != nil {
			return cfg, nil, err
		}
		overlay.Lower = append(overlay.Lower, empty)
	}
	cfg.Overlay = &overlay
	return cfg, nil, nil
}

// CopyMain only changes the filesystem root of this auxiliary process.
func CopyMain() {
	runtime.LockOSThread()
	conn := os.NewFile(3, "copy")
	defer conn.Close()
	var cfg copyConfig
	if err := json.NewDecoder(os.Stdin).Decode(&cfg); err != nil {
		copyReply(conn, err)
		return
	}
	if cfg.Spec.Op == guestwire.CopyDiff || cfg.Spec.Op == guestwire.CopyCommit {
		if err := sendWritableLayer(conn, cfg); err != nil {
			fmt.Fprintln(os.Stderr, "read container writable layer:", err)
		}
		return
	}
	if err := enterCopyRoot(cfg); err != nil {
		copySetupFailed(conn, cfg.Spec, err)
		return
	}
	if cfg.Spec.Op == guestwire.CopyIn {
		source := &guestwire.CopyChunkReader{Source: conn}
		dest, err := os.OpenRoot(cfg.Spec.Path)
		if err == nil {
			err = extractContainerArchive(dest, source, cfg.Spec.Flags)
			dest.Close()
		}
		// Drain the upload even after a filesystem error. Otherwise a
		// client still writing a large archive cannot read the reply.
		_, drainErr := io.Copy(io.Discard, source)
		copyReply(conn, errors.Join(err, drainErr))
		return
	}
	if cfg.Spec.Op == guestwire.CopyExport {
		if err := exportContainerArchive(conn, cfg.Spec.Flags&guestwire.CopyChunked != 0); err != nil {
			fmt.Fprintln(os.Stderr, "export container archive:", err)
		}
		return
	}
	if err := sendContainerArchive(conn, cfg.Spec); err != nil {
		fmt.Fprintln(os.Stderr, "copy container archive:", err)
	}
}

func enterCopyRoot(cfg copyConfig) error {
	if cfg.Overlay == nil {
		root := 4
		defer unix.Close(root)
		if cfg.Spec.Op == guestwire.CopyExport {
			var err error
			root, err = cloneExportRoot(root, 5)
			if err != nil {
				return err
			}
			defer unix.Close(root)
		}
		err := unix.Fchdir(root)
		if err != nil {
			return err
		}
	} else {
		if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""); err != nil {
			return err
		}
		if err := mountOverlay(*cfg.Overlay, unix.MS_NOSUID|unix.MS_NODEV); err != nil {
			return err
		}
		if err := unix.Chdir(cfg.Overlay.Root); err != nil {
			return err
		}
	}
	if err := unix.Chroot("."); err != nil {
		return err
	}
	return unix.Chdir("/")
}

func cloneExportRoot(root, namespace int) (int, error) {
	defer unix.Close(namespace)
	if err := unix.Unshare(unix.CLONE_FS); err != nil {
		return -1, err
	}
	if err := unix.Setns(namespace, unix.CLONE_NEWNS); err != nil {
		return -1, fmt.Errorf("join container mount namespace: %w", err)
	}
	// Clone only the root mount from its own namespace. Submounts
	// such as bind mounts and volumes stay outside the archive.
	tree, err := unix.OpenTree(root, "", unix.OPEN_TREE_CLONE|unix.OPEN_TREE_CLOEXEC|unix.AT_EMPTY_PATH)
	if err != nil {
		return -1, fmt.Errorf("clone container root mount: %w", err)
	}
	return tree, nil
}

func copySetupFailed(conn io.ReadWriter, spec guestwire.CopySpec, err error) {
	if spec.Op == guestwire.CopyIn {
		_, _ = io.Copy(io.Discard, &guestwire.CopyChunkReader{Source: conn})
	}
	copyReply(conn, err)
}

func copyReply(conn io.Writer, err error) {
	frame := guestwire.Frame{Kind: guestwire.CopyDoneMessage}
	if err != nil {
		message := err.Error()
		switch {
		case errors.Is(err, fs.ErrNotExist):
			message = "notfound: " + message
		case errors.Is(err, unix.ENOTDIR):
			message = "notdir: " + message
		}
		frame = guestwire.Frame{Kind: guestwire.ErrorMessage, Payload: []byte(message)}
	}
	_ = guestwire.WriteFrame(conn, frame)
}

func sendContainerArchive(conn io.Writer, spec guestwire.CopySpec) error {
	source := filepath.Clean(spec.Path)
	parent, name := filepath.Dir(source), filepath.Base(source)
	if source == "/" {
		name = "."
	}
	root, err := os.OpenRoot(parent)
	if err != nil {
		copyReply(conn, err)
		return nil
	}
	defer root.Close()
	info, err := root.Lstat(name)
	if err != nil {
		copyReply(conn, err)
		return nil
	}
	stat := info.Sys().(*syscall.Stat_t)
	header := guestwire.ArchiveStat{
		Name: filepath.Base(source), Size: uint64(info.Size()), Mode: stat.Mode, Inode: stat.Ino,
		Sec: info.ModTime().Unix(), Nsec: uint32(info.ModTime().Nanosecond()),
	}
	if info.Mode()&os.ModeSymlink != 0 {
		header.Link, err = root.Readlink(name)
		if err != nil {
			copyReply(conn, err)
			return nil
		}
	}
	frame, err := header.Frame()
	if err != nil {
		copyReply(conn, err)
		return nil
	}
	if err := guestwire.WriteFrame(conn, frame); err != nil || spec.Op == guestwire.CopyStat {
		return err
	}
	tw := tar.NewWriter(conn)
	links := make(map[[2]uint64]string)
	if !info.IsDir() {
		if err := appendArchiveEntry(tw, root, name, fs.FileInfoToDirEntry(info), links); err != nil {
			return err
		}
		return tw.Close()
	}
	err = fs.WalkDir(root.FS(), name, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Type()&os.ModeSocket != 0 {
			return nil
		}
		full := filepath.Join(parent, path)
		if full == "/proc" || full == "/sys" || full == "/dev" {
			return fs.SkipDir
		}
		return appendArchiveEntry(tw, root, path, entry, links)
	})
	if err != nil {
		return err
	}
	return tw.Close()
}

func exportContainerArchive(conn io.Writer, chunked bool) error {
	root, err := os.OpenRoot("/")
	if err != nil {
		copyReply(conn, err)
		return err
	}
	defer root.Close()
	return streamCopyResult(conn, chunked, func(dst io.Writer) error { return writeRootArchive(dst, root) })
}

func streamCopyResult(conn io.Writer, chunked bool, writeResult func(io.Writer) error) error {
	if err := guestwire.WriteFrame(conn, guestwire.Frame{Kind: guestwire.CopyDoneMessage}); err != nil {
		return err
	}
	if !chunked {
		return writeResult(conn)
	}
	stream := guestwire.CopyChunkWriter{Target: conn}
	buf := bufio.NewWriterSize(stream, 256<<10)
	if err := writeResult(buf); err != nil {
		return err
	}
	if err := buf.Flush(); err != nil {
		return err
	}
	return stream.Close()
}

func writeRootArchive(dst io.Writer, root *os.Root) error {
	tw := tar.NewWriter(dst)
	links := make(map[[2]uint64]string)
	err := fs.WalkDir(root.FS(), ".", func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == "." || entry.Type()&os.ModeSocket != 0 {
			return nil
		}
		return appendArchiveEntry(tw, root, path, entry, links)
	})
	if err != nil {
		return err
	}
	return tw.Close()
}

func appendArchiveEntry(tw *tar.Writer, root *os.Root, name string, entry fs.DirEntry, links map[[2]uint64]string) error {
	info, err := entry.Info()
	if err != nil {
		return err
	}
	var target string
	if entry.Type()&os.ModeSymlink != 0 {
		target, err = root.Readlink(name)
		if err != nil {
			return err
		}
	}
	header, err := tar.FileInfoHeader(info, target)
	if err != nil {
		return err
	}
	header.Name = name
	if header.Typeflag == tar.TypeDir {
		header.Name += "/"
	}
	if st, ok := info.Sys().(*syscall.Stat_t); ok && header.Typeflag == tar.TypeReg && st.Nlink > 1 {
		key := [2]uint64{uint64(st.Dev), st.Ino}
		if first, ok := links[key]; ok {
			header.Typeflag, header.Linkname, header.Size = tar.TypeLink, first, 0
		} else {
			links[key] = name
		}
	}
	if header.Typeflag != tar.TypeReg {
		return tw.WriteHeader(header)
	}
	f, err := root.OpenFile(name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	current, err := f.Stat()
	if err != nil {
		return err
	}
	if !current.Mode().IsRegular() || !os.SameFile(info, current) {
		return fmt.Errorf("file changed during archive: %s", name)
	}
	if err := tw.WriteHeader(header); err != nil {
		return err
	}
	_, err = io.CopyN(tw, f, header.Size)
	return err
}

func containerArchiveName(raw string) (string, error) {
	if !filepath.IsLocal(raw) {
		return "", fmt.Errorf("invalid archive path %q", raw)
	}
	for part := range strings.SplitSeq(raw, "/") {
		if part == ".." {
			return "", fmt.Errorf("invalid archive path %q", raw)
		}
	}
	return filepath.Clean(raw), nil
}
