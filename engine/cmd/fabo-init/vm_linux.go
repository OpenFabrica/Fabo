package main

import (
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/OpenFabrica/FabriBay/engine/internal/guestagent"
	"github.com/OpenFabrica/FabriBay/engine/internal/guestwire"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

const vmStage = "__fabo-vm"

func runVM() error {
	unix.Umask(022)
	if len(os.Args) == 1 {
		started, err := guestagent.BoottimeNS()
		if err != nil {
			return err
		}
		return switchRoot(started)
	}
	if len(os.Args) != 3 || os.Args[1] != vmStage {
		return fmt.Errorf("invalid Fabo VM boot stage")
	}
	started, err := strconv.ParseUint(os.Args[2], 10, 64)
	if err != nil {
		return err
	}
	if err := prepareVM(); err != nil {
		return err
	}
	rosetta := enableRosetta()
	const data = "/var/lib/fabo"
	_, diskErr := os.Stat("/dev/vda")
	volumeDisk := diskErr == nil
	if volumeDisk {
		defer func() {
			if err := unix.Unmount(data, 0); err != nil {
				log.Printf("unmount VM disk: %v", err)
			}
		}()
	}
	stopNetwork, err := startUplink()
	if err != nil {
		return err
	}
	defer stopNetwork()
	stopRelays, err := startPortRelays()
	if err != nil {
		return err
	}
	defer stopRelays()
	stopHost, err := startHostBound()
	if err != nil {
		return err
	}
	defer stopHost()
	stopEngine, err := startEngineSocket()
	if err != nil {
		return err
	}
	defer stopEngine()
	if stopUSB, err := startUSB(); err != nil {
		log.Printf("USB passthrough unavailable: %v", err)
	} else {
		defer stopUSB()
	}
	conn, err := connectHost()
	if err != nil {
		return err
	}
	defer conn.Close()
	volumeRoot := ""
	if volumeDisk {
		volumeRoot = guestwire.VolumeRoot
	}
	return serve(conn, started, filepath.Join(data, "layers"), filepath.Join(data, "containers"), "/sys/fs/cgroup/fabo", setVMClock, volumeRoot, rosetta)
}

func enableRosetta() bool {
	if runtime.GOARCH != "arm64" {
		return false
	}
	cmdline, err := os.ReadFile("/proc/cmdline")
	if err != nil || !strings.Contains(" "+strings.TrimSpace(string(cmdline))+" ", " fabo.rosetta=1 ") {
		return false
	}
	if err := registerRosetta(); err != nil {
		log.Printf("Rosetta unavailable: %v", err)
		return false
	}
	return true
}

func registerRosetta() error {
	const binfmt = "/proc/sys/fs/binfmt_misc"
	if err := mountVM("rosetta", "/run/rosetta", "virtiofs", unix.MS_RDONLY, ""); err != nil {
		return err
	}
	if err := mountVM("binfmt_misc", binfmt, "binfmt_misc", 0, ""); err != nil {
		return err
	}
	// Match x86-64 executable and PIE ELF headers. F pins the interpreter
	// outside container roots so it remains available across pivot_root.
	const magic = `\x7fELF\x02\x01\x01\x00\x00\x00\x00\x00\x00\x00\x00\x00\x02\x00\x3e\x00`
	const mask = `\xff\xff\xff\xff\xff\xfe\xfe\x00\xff\xff\xff\xff\xff\xff\xff\xff\xfe\xff\xff\xff`
	return os.WriteFile(binfmt+"/register", []byte(":rosetta:M::"+magic+":"+mask+":/run/rosetta/rosetta:CF"), 0)
}

// The kernel refuses pivot_root from initramfs. Switch PID 1 to a tmpfs
// root and exec the copied binary so container setup can re-exec /init.
func switchRoot(started uint64) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if err := mountVM("root", "/root", "tmpfs", 0, "mode=0755"); err != nil {
		return err
	}
	if err := copyInit(); err != nil {
		return err
	}
	if err := os.Remove("/init"); err != nil {
		return err
	}
	if err := unix.Chdir("/root"); err != nil {
		return err
	}
	if err := unix.Mount(".", "/", "", unix.MS_MOVE, ""); err != nil {
		return fmt.Errorf("move VM root: %w", err)
	}
	if err := unix.Chroot("."); err != nil {
		return err
	}
	if err := unix.Chdir("/"); err != nil {
		return err
	}
	return unix.Exec("/init", []string{"/init", vmStage, strconv.FormatUint(started, 10)}, os.Environ())
}

func copyInit() error {
	source, err := os.Open("/init")
	if err != nil {
		return err
	}
	defer source.Close()
	target, err := os.OpenFile("/root/init", os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0755)
	if err != nil {
		return err
	}
	_, err = io.Copy(target, source)
	return errors.Join(err, target.Close())
}

func mountVM(source, target, fs string, flags uintptr, options string) error {
	if err := os.MkdirAll(target, 0755); err != nil {
		return err
	}
	if err := unix.Mount(source, target, fs, flags, options); err != nil {
		return fmt.Errorf("mount %s at %s: %w", fs, target, err)
	}
	return nil
}

func prepareVM() error {
	const restricted = unix.MS_NOSUID | unix.MS_NODEV | unix.MS_NOEXEC
	for _, mount := range []struct {
		source, target, fs string
		flags              uintptr
		options            string
	}{
		{"proc", "/proc", "proc", restricted, ""},
		{"sysfs", "/sys", "sysfs", restricted, ""},
		{"devtmpfs", "/dev", "devtmpfs", unix.MS_NOSUID, "mode=0755"},
		{"cgroup2", "/sys/fs/cgroup", "cgroup2", restricted, "nsdelegate,memory_recursiveprot"},
		{"tmpfs", "/run", "tmpfs", unix.MS_NOSUID | unix.MS_NODEV, "mode=0755"},
		{"tmpfs", "/tmp", "tmpfs", unix.MS_NOSUID | unix.MS_NODEV, "mode=1777"},
	} {
		if err := mountVM(mount.source, mount.target, mount.fs, mount.flags, mount.options); err != nil {
			return err
		}
	}
	if err := os.MkdirAll("/etc", 0755); err != nil {
		return err
	}
	if err := unix.Sethostname([]byte("fabo")); err != nil {
		return err
	}
	lo, err := netlink.LinkByName("lo")
	if err != nil {
		return err
	}
	if err := netlink.LinkSetUp(lo); err != nil {
		return err
	}
	const cgroup = "/sys/fs/cgroup/fabo"
	if err := os.MkdirAll(cgroup, 0755); err != nil {
		return err
	}
	for _, path := range []string{"/sys/fs/cgroup", cgroup} {
		if err := os.WriteFile(filepath.Join(path, "cgroup.subtree_control"), []byte("+cpu +memory +pids"), 0); err != nil {
			return err
		}
		for _, controller := range []string{"+cpuset", "+io"} {
			_ = os.WriteFile(filepath.Join(path, "cgroup.subtree_control"), []byte(controller), 0)
		}
	}
	cmdline, err := os.ReadFile("/proc/cmdline")
	if err != nil {
		return err
	}
	for _, arg := range strings.Fields(string(cmdline)) {
		if arg == "fabo.share=1" {
			if err := mountVM(guestwire.HostShareTag, guestwire.HostSharePath, "virtiofs", unix.MS_NOSUID|unix.MS_NODEV|unix.MS_NOATIME, ""); err != nil {
				return err
			}
			break
		}
	}
	const disk = "/dev/vda"
	if _, err := os.Stat(disk); err == nil {
		return mountVM(disk, "/var/lib/fabo", "ext4", unix.MS_NOATIME, "noinit_itable")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func connectHost() (*os.File, error) {
	var last error
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
		if err != nil {
			return nil, err
		}
		addr := &unix.SockaddrVM{CID: unix.VMADDR_CID_HOST, Port: guestwire.Port}
		if err := unix.Connect(fd, addr); err == nil {
			if err := unix.SetNonblock(fd, true); err != nil {
				_ = unix.Close(fd)
				return nil, err
			}
			return os.NewFile(uintptr(fd), "fabo-vsock"), nil
		} else {
			last = err
			_ = unix.Close(fd)
		}
		time.Sleep(25 * time.Millisecond)
	}
	return nil, fmt.Errorf("dial host vsock: %w", last)
}

// setVMClock is wired only in VM PID 1, which owns the guest's clock.
func setVMClock(now time.Time) error {
	if drift := time.Since(now); drift >= -time.Second && drift <= time.Second {
		return nil
	}
	ts := unix.NsecToTimespec(now.UnixNano())
	return unix.ClockSettime(unix.CLOCK_REALTIME, &ts)
}
