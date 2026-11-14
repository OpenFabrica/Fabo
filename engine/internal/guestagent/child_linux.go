package guestagent

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"

	"github.com/OpenFabrica/Fabo/engine/internal/guestwire"
)

// ContainerMain is invoked only by the agent's namespace child.
func ContainerMain(join bool) {
	// These syscalls change the calling thread. It is also the thread that
	// replaces the process with the container command.
	runtime.LockOSThread()
	status := os.NewFile(3, "container-status")
	if status == nil {
		os.Exit(127)
	}
	unix.CloseOnExec(3)
	var err error
	if join {
		err = executeInContainer()
	} else {
		err = executeContainer(status)
	}
	if err != nil {
		var command *commandError
		if join && errors.As(err, &command) {
			// The agent waits for the bootstrap pipe before draining output.
			_ = status.Close()
			fmt.Fprintln(os.Stderr, command.err)
			os.Exit(command.code)
		}
		fmt.Fprintln(status, err)
	}
	os.Exit(127)
}

func executeContainer(status *os.File) error {
	var cfg processConfig
	if err := json.NewDecoder(os.Stdin).Decode(&cfg); err != nil {
		return err
	}
	if cfg.JoinNetwork {
		if err := enterSharedNetwork(cfg.Spec.Flags); err != nil {
			return err
		}
	}
	if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""); err != nil {
		return fmt.Errorf("make mounts private: %w", err)
	}
	mounts, err := openMounts(cfg.Spec.Mounts, cfg.VM)
	if err != nil {
		return err
	}
	defer closeMounts(mounts)
	// Match the Rust guest's set-ID behavior on the container root. The
	// process policy below separately applies explicit no-new-privileges.
	if err := mountOverlay(cfg, unix.MS_NODEV); err != nil {
		return err
	}
	if err := populateVolumes(cfg.Root, mounts); err != nil {
		return err
	}
	if err := unix.Chdir(cfg.Root); err != nil {
		return err
	}
	if err := unix.PivotRoot(".", "."); err != nil {
		return fmt.Errorf("pivot root: %w", err)
	}
	if err := unix.Unmount(".", unix.MNT_DETACH); err != nil {
		return fmt.Errorf("detach original root: %w", err)
	}
	if err := unix.Chdir("/"); err != nil {
		return err
	}
	if cfg.Spec.Hostname != "" {
		if err := unix.Sethostname([]byte(cfg.Spec.Hostname)); err != nil {
			return err
		}
	}
	if cfg.Spec.Domainname != "" {
		if err := unix.Setdomainname([]byte(cfg.Spec.Domainname)); err != nil {
			return fmt.Errorf("set container domain name: %w", err)
		}
	}
	privileged := cfg.Spec.Flags&guestwire.RunPrivileged != 0
	if err := mountContainerFilesystems(cfg.Cgroup, privileged, cfg.Spec.Limits.ShmSize); err != nil {
		return err
	}
	if err := makePrivilegedDevices(cfg.HostDevices); err != nil {
		return err
	}
	if err := mountDeviceNodes(cfg.DeviceNodes); err != nil {
		return err
	}
	if err := applySysctls(cfg.Spec.Sysctls, !cfg.JoinNetwork && cfg.Spec.PrivateNetwork()); err != nil {
		return err
	}
	if !privileged {
		if err := protectProcSys(); err != nil {
			return fmt.Errorf("protect container sysctl mount: %w", err)
		}
	}
	if err := attachMounts(mounts); err != nil {
		return err
	}
	if cfg.Spec.Flags&guestwire.RunReadOnlyRoot != 0 {
		if err := unix.MountSetattr(unix.AT_FDCWD, "/", 0, &unix.MountAttr{Attr_set: unix.MOUNT_ATTR_RDONLY}); err != nil {
			return fmt.Errorf("make root filesystem read-only: %w", err)
		}
	}
	return executeProgram(cfg.Spec, status)
}

func mountOverlay(cfg processConfig, flags uintptr) error {
	for _, dir := range append(append([]string(nil), cfg.Lower...), cfg.Upper, cfg.Work) {
		if strings.ContainsAny(dir, ":,\\") {
			return fmt.Errorf("unsupported overlay path")
		}
	}
	options := "lowerdir=" + strings.Join(cfg.Lower, ":")
	if cfg.Upper == "" {
		flags |= unix.MS_RDONLY
	} else {
		options += ",upperdir=" + cfg.Upper + ",workdir=" + cfg.Work
	}
	if err := unix.Mount("overlay", cfg.Root, "overlay", flags, options); err != nil {
		return fmt.Errorf("mount overlay: %w", err)
	}
	return nil
}

func executeProgram(spec guestwire.RunSpec, status *os.File) error {
	user, err := containerIdentity(spec.User, spec.Process.Groups)
	if err != nil {
		return err
	}
	tty := spec.Flags&guestwire.RunTTY != 0
	if tty {
		if err := attachTerminal(user.uid, status != nil); err != nil {
			return fmt.Errorf("attach terminal: %w", err)
		}
	} else {
		input := 4 // fd inherited after the bootstrap status pipe
		if spec.Flags&guestwire.RunStdin == 0 {
			var err error
			input, err = unix.Open("/dev/null", unix.O_RDONLY, 0)
			if err != nil {
				return err
			}
		}
		if input != 0 {
			err := unix.Dup3(input, 0, 0)
			_ = unix.Close(input)
			if err != nil {
				return err
			}
		}
	}
	if err := applyProcessLimits(spec.Process.Ulimits); err != nil {
		return err
	}
	if err := giveStdio(user.uid); err != nil {
		return fmt.Errorf("set standard stream ownership: %w", err)
	}
	privileged := spec.Flags&guestwire.RunPrivileged != 0
	init := status != nil && spec.Flags&guestwire.RunInit != 0
	capabilities := guestwire.EffectiveCapabilities(spec.Caps)
	if spec.Flags&guestwire.RunNoNewPrivs != 0 {
		if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
			return fmt.Errorf("set no-new-privileges: %w", err)
		}
	}
	// Install before dropping CAP_SYS_ADMIN so the filter can also be used
	// without forcing no-new-privileges.
	unconfined := privileged || spec.Flags&guestwire.RunNoSeccomp != 0
	if err := restrictSyscalls(unconfined, init, capabilities); err != nil {
		return err
	}
	if err := setIdentity(user, privileged, capabilities, init); err != nil {
		return err
	}
	cwd := spec.Cwd
	if cwd == "" {
		cwd = "/"
	}
	if err := unix.Chdir(cwd); err != nil {
		if err := os.MkdirAll(cwd, 0755); err != nil {
			return fmt.Errorf("create working directory: %w", err)
		}
		if err := unix.Chdir(cwd); err != nil {
			return fmt.Errorf("enter working directory: %w", err)
		}
	}
	env := spec.Env
	path := "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
	hasPath := false
	for _, kv := range env {
		if value, ok := strings.CutPrefix(kv, "PATH="); ok {
			path = value
			hasPath = true
			break
		}
	}
	if !hasPath {
		env = append(env, "PATH="+path)
	}
	if err := os.Setenv("PATH", path); err != nil {
		return err
	}
	program, err := exec.LookPath(spec.Argv[0])
	if err == nil {
		if init {
			err = runInit(status, program, spec.Argv, env, tty)
		} else {
			err = unix.Exec(program, spec.Argv, env)
		}
	}
	code := 126
	if errors.Is(err, exec.ErrNotFound) || errors.Is(err, os.ErrNotExist) {
		code = 127
	}
	return &commandError{err: err, code: code}
}

type commandError struct {
	err  error
	code int
}

func (e *commandError) Error() string { return e.err.Error() }
func (e *commandError) Unwrap() error { return e.err }

func mountContainerFilesystems(cgroup, privileged bool, shmSize uint64) error {
	for _, dir := range []string{"/proc", "/dev", "/sys"} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return err
		}
		info, err := os.Lstat(dir)
		if err != nil || !info.IsDir() {
			return fmt.Errorf("%s must be a directory", dir)
		}
	}
	if err := unix.Mount("proc", "/proc", "proc", unix.MS_NOSUID|unix.MS_NODEV|unix.MS_NOEXEC, ""); err != nil {
		return fmt.Errorf("mount proc: %w", err)
	}
	if err := unix.Mount("tmpfs", "/dev", "tmpfs", unix.MS_NOSUID|unix.MS_NOEXEC, "mode=0755,size=1m"); err != nil {
		return fmt.Errorf("mount dev: %w", err)
	}
	for _, device := range []struct {
		name  string
		minor uint32
	}{{"null", 3}, {"zero", 5}, {"full", 7}, {"random", 8}, {"urandom", 9}} {
		if err := unix.Mknod("/dev/"+device.name, unix.S_IFCHR|0666, int(unix.Mkdev(1, device.minor))); err != nil {
			return err
		}
		if err := unix.Chmod("/dev/"+device.name, 0666); err != nil {
			return err
		}
	}
	for _, link := range []struct{ name, target string }{
		{"fd", "/proc/self/fd"}, {"stdin", "/proc/self/fd/0"},
		{"stdout", "/proc/self/fd/1"}, {"stderr", "/proc/self/fd/2"},
	} {
		if err := os.Symlink(link.target, "/dev/"+link.name); err != nil {
			return fmt.Errorf("create standard stream link: %w", err)
		}
	}
	if err := mountTerminalDevices(); err != nil {
		return fmt.Errorf("mount terminal devices: %w", err)
	}
	if shmSize == 0 {
		shmSize = 64 << 20
	}
	if err := os.Mkdir("/dev/shm", 0755); err != nil {
		return err
	}
	if err := unix.Mount("tmpfs", "/dev/shm", "tmpfs", unix.MS_NOSUID|unix.MS_NODEV|unix.MS_NOEXEC, fmt.Sprintf("mode=1777,size=%d", shmSize)); err != nil {
		return fmt.Errorf("mount shared memory: %w", err)
	}
	sysfsFlags := uintptr(unix.MS_NOSUID | unix.MS_NODEV | unix.MS_NOEXEC)
	if !privileged {
		sysfsFlags |= unix.MS_RDONLY
	}
	if err := unix.Mount("sysfs", "/sys", "sysfs", sysfsFlags, ""); err != nil {
		return fmt.Errorf("mount container sysfs: %w", err)
	}
	if cgroup {
		if err := os.MkdirAll("/sys/fs/cgroup", 0755); err != nil {
			return err
		}
		flags := uintptr(unix.MS_NOSUID | unix.MS_NODEV | unix.MS_NOEXEC)
		if !privileged {
			flags |= unix.MS_RDONLY
		}
		if err := unix.Mount("cgroup2", "/sys/fs/cgroup", "cgroup2", flags, ""); err != nil {
			return fmt.Errorf("mount container cgroup: %w", err)
		}
	}

	return nil
}

// giveStdio follows the original agent's pipe and terminal ownership rule.
// /dev/null and other inherited files keep their existing owner and mode.
func giveStdio(uid int) error {
	for fd := 0; fd < 3; fd++ {
		var stat unix.Stat_t
		if err := unix.Fstat(fd, &stat); err != nil {
			return err
		}
		if stat.Uid == uint32(uid) {
			continue
		}
		if stat.Mode&unix.S_IFMT != unix.S_IFIFO {
			if _, err := unix.IoctlGetTermios(fd, unix.TCGETS); err != nil {
				continue
			}
		}
		if err := unix.Fchown(fd, uid, -1); err != nil && err != unix.EINVAL && err != unix.EPERM && err != unix.EROFS {
			return err
		}
	}
	return nil
}

func setIdentity(user identity, privileged bool, capabilities uint64, init bool) error {
	if err := unix.Setgroups(user.groups); err != nil {
		return fmt.Errorf("drop supplementary groups: %w", err)
	}
	var keep uint64
	header := unix.CapUserHeader{Version: unix.LINUX_CAPABILITY_VERSION_3}
	if !privileged {
		var current [2]unix.CapUserData
		if err := unix.Capget(&header, &current[0]); err != nil {
			return err
		}
		permitted := uint64(current[0].Permitted) | uint64(current[1].Permitted)<<32
		keep = permitted & capabilities
	}
	for cap := 0; !privileged; cap++ {
		present, err := unix.PrctlRetInt(unix.PR_CAPBSET_READ, uintptr(cap), 0, 0, 0)
		if err == unix.EINVAL {
			break
		} else if err != nil {
			return err
		}
		if present == 0 {
			keep &^= 1 << cap
			continue
		}
		if keep&(1<<cap) != 0 {
			continue
		}
		if init {
			// The Go PID 1 remains multithreaded after starting the workload.
			// A normal bootstrap replaces its calling thread and exits the rest.
			_, _, err := syscall.AllThreadsSyscall6(unix.SYS_PRCTL, unix.PR_CAPBSET_DROP, uintptr(cap), 0, 0, 0, 0)
			if err != 0 {
				return fmt.Errorf("drop init capability %d: %w", cap, err)
			}
		} else if err := unix.Prctl(unix.PR_CAPBSET_DROP, uintptr(cap), 0, 0, 0); err != nil {
			return fmt.Errorf("drop capability %d: %w", cap, err)
		}
	}
	if err := unix.Setresgid(user.gid, user.gid, user.gid); err != nil {
		return fmt.Errorf("set container GID: %w", err)
	}
	if err := unix.Setresuid(user.uid, user.uid, user.uid); err != nil {
		return fmt.Errorf("set container UID: %w", err)
	}
	if !privileged {
		if user.uid != 0 {
			keep = 0
		}
		data := [2]unix.CapUserData{
			{Effective: uint32(keep), Permitted: uint32(keep)},
			{Effective: uint32(keep >> 32), Permitted: uint32(keep >> 32)},
		}
		if init {
			_, _, err := syscall.AllThreadsSyscall(unix.SYS_CAPSET, uintptr(unsafe.Pointer(&header)), uintptr(unsafe.Pointer(&data[0])), 0)
			if err != 0 {
				return fmt.Errorf("set init capabilities: %w", err)
			}
		} else if err := unix.Capset(&header, &data[0]); err != nil {
			return err
		}
	}
	return nil
}
