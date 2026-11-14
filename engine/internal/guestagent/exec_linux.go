package guestagent

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/OpenFabrica/Fabo/engine/internal/guestwire"
)

var execNamespaces = []struct {
	name string
	flag int
}{
	{"net", unix.CLONE_NEWNET},
	{"uts", unix.CLONE_NEWUTS},
	{"ipc", unix.CLONE_NEWIPC},
	{"cgroup", unix.CLONE_NEWCGROUP},
	{"mnt", unix.CLONE_NEWNS},
}

func (r *Runtime) Exec(spec guestwire.ExecSpec, emit func(guestwire.RunEvent) error, ack func(string, uint32) error) error {
	start := time.Now()
	if !validContainerID(spec.ContainerID) || !validContainerID(spec.ID) || len(spec.Argv) == 0 {
		return fmt.Errorf("invalid exec ID or command")
	}
	var output *execOutput
	var processGroup bool
	for _, option := range spec.Options {
		switch option {
		case guestwire.ExecOutputCreditOption:
			output = newExecOutput()
		case guestwire.ExecProcessGroupOption:
			processGroup = true
		default:
			return fmt.Errorf("unsupported Go guest exec option %q", option)
		}
	}
	if output != nil {
		emit = output.wrap(emit)
	}
	if processGroup && spec.Flags&guestwire.RunTTY != 0 {
		return fmt.Errorf("a process group cannot use an exec terminal")
	}
	if spec.Flags & ^(guestwire.RunTTY|guestwire.RunStdin|guestwire.RunPrivileged|guestwire.RunSecurityFlags) != 0 {
		return fmt.Errorf("unsupported Go guest exec options")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	target := r.children[spec.ContainerID]
	if target == nil || target.isExec || r.children[spec.ID] != nil {
		return fmt.Errorf("exec requires a running container and a new process ID")
	}
	// The exec joins the target's root and namespaces below.
	if err := target.requireSettled(spec.ContainerID); err != nil {
		return err
	}

	spec.Caps = target.caps
	spec.Flags = spec.Flags&^guestwire.RunSecurityFlags | target.security
	if target.privileged {
		spec.Flags |= guestwire.RunPrivileged
	}
	proc, err := os.Open(fmt.Sprintf("/proc/%d", target.Process.Pid))
	if err != nil {
		return fmt.Errorf("open container process: %w", err)
	}
	defer proc.Close()
	open := func(name string, flags int) (*os.File, error) {
		fd, err := unix.Openat(int(proc.Fd()), name, flags|unix.O_CLOEXEC, 0)
		if err != nil {
			return nil, fmt.Errorf("open container %s: %w", name, err)
		}
		return os.NewFile(uintptr(fd), name), nil
	}
	pidNS, err := open("ns/pid", unix.O_RDONLY)
	if err != nil {
		return err
	}
	defer pidNS.Close()
	files := make([]*os.File, 0, len(execNamespaces)+1)
	for _, ns := range execNamespaces {
		f, err := open("ns/"+ns.name, unix.O_RDONLY)
		if err != nil {
			return err
		}
		defer f.Close()
		files = append(files, f)
	}
	root, err := open("root", unix.O_PATH|unix.O_DIRECTORY)
	if err != nil {
		return err
	}
	defer root.Close()
	files = append(files, root)
	var cgroup *os.File
	if target.cgroup != "" {
		cgroupPath := target.cgroup
		if target.privileged {
			// BuildKit enables controllers in the container's cgroup
			// root and moves itself to a child group. Exec must use
			// that child because the root can no longer host processes.
			info, err := open("cgroup", unix.O_RDONLY)
			if err != nil {
				return err
			}
			data, err := io.ReadAll(info)
			info.Close()
			if err != nil {
				return err
			}
			cgroupPath, err = liveExecCgroup(cgroupPath, string(data))
			if err != nil {
				return err
			}
		}
		cgroup, err = os.Open(cgroupPath)
		if err != nil {
			return fmt.Errorf("open container cgroup: %w", err)
		}
		defer cgroup.Close()
	}
	// The proc handles must belong to the original child. os.Process
	// checks that process rather than sending the signal to a reused PID.
	if err := target.Process.Signal(syscall.Signal(0)); err != nil {
		return fmt.Errorf("check container process: %w", err)
	}
	launch := processLaunch{
		mode: "__fabo-exec", id: spec.ID, flags: spec.Flags, config: spec,
		attributes: &syscall.SysProcAttr{Setpgid: processGroup}, cgroup: cgroup, files: files,
		start: func(cmd *exec.Cmd) error { return startInPIDNamespace(cmd, pidNS) },
	}
	child, err := launchProcess(launch, emit, ack)
	if err != nil {
		return err
	}
	child.isExec, child.execOutput = true, output
	return r.trackProcess(spec.ID, child, start, emit, nil, nil)
}

func liveExecCgroup(root, membership string) (string, error) {
	for line := range strings.SplitSeq(membership, "\n") {
		path, ok := strings.CutPrefix(line, "0::")
		if !ok || !filepath.IsAbs(path) {
			continue
		}
		current := filepath.Join("/sys/fs/cgroup", path)
		relative, err := filepath.Rel(root, current)
		if err != nil || !filepath.IsLocal(relative) {
			return "", fmt.Errorf("container process moved outside its cgroup")
		}
		return current, nil
	}
	return "", fmt.Errorf("container process has no cgroup v2 membership")
}

// setns for PID affects only children of this thread. A dedicated goroutine
// prevents a failure restoring the namespace from affecting later forks.
func startInPIDNamespace(cmd *exec.Cmd, target *os.File) error {
	result := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		original, err := os.Open("/proc/thread-self/ns/pid_for_children")
		if err != nil {
			runtime.UnlockOSThread()
			result <- err
			return
		}
		defer original.Close()
		if err := unix.Setns(int(target.Fd()), unix.CLONE_NEWPID); err != nil {
			runtime.UnlockOSThread()
			result <- fmt.Errorf("join container PID namespace: %w", err)
			return
		}
		startErr := cmd.Start()
		if err := unix.Setns(int(original.Fd()), unix.CLONE_NEWPID); err != nil {
			result <- errors.Join(startErr, fmt.Errorf("restore launcher PID namespace: %w", err))
			return
		}
		runtime.UnlockOSThread()
		result <- startErr
	}()
	return <-result
}

func executeInContainer() error {
	var spec guestwire.ExecSpec
	if err := json.NewDecoder(os.Stdin).Decode(&spec); err != nil {
		return err
	}
	// Go's runtime threads normally share the filesystem context. Isolate
	// this thread before joining the mount namespace and changing its root.
	if err := unix.Unshare(unix.CLONE_FS); err != nil {
		return err
	}
	fd := 4
	if spec.Flags&(guestwire.RunTTY|guestwire.RunStdin) != 0 {
		fd++
	}
	for _, ns := range execNamespaces {
		err := unix.Setns(fd, ns.flag)
		_ = unix.Close(fd)
		if err != nil {
			return fmt.Errorf("join container %s namespace: %w", ns.name, err)
		}
		fd++
	}
	if err := unix.Fchdir(fd); err != nil {
		return fmt.Errorf("enter container root: %w", err)
	}
	_ = unix.Close(fd)
	if err := unix.Chroot("."); err != nil {
		return fmt.Errorf("set container root: %w", err)
	}
	if err := unix.Chdir("/"); err != nil {
		return err
	}
	return executeProgram(guestwire.RunSpec{Argv: spec.Argv, Env: spec.Env, Cwd: spec.Cwd, User: spec.User, Flags: spec.Flags, Process: spec.Process, Caps: spec.Caps}, nil)
}
