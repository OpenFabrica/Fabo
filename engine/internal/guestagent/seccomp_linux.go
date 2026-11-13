package guestagent

import (
	"fmt"
	"runtime"
	"unsafe"

	"golang.org/x/sys/unix"
)

type capabilitySyscall struct {
	number     uint32
	capability int
}

// capabilitySyscalls is Rust Kiri's default syscall policy. A negative
// capability means the syscall is always denied by the default profile.
func capabilitySyscalls() []capabilitySyscall {
	return []capabilitySyscall{
		{unix.SYS_ADD_KEY, -1},
		{unix.SYS_KEYCTL, -1},
		{unix.SYS_SYSLOG, -1},
		{unix.SYS_REQUEST_KEY, -1},
		{unix.SYS_INIT_MODULE, unix.CAP_SYS_MODULE},
		{unix.SYS_FINIT_MODULE, unix.CAP_SYS_MODULE},
		{unix.SYS_DELETE_MODULE, unix.CAP_SYS_MODULE},
		{unix.SYS_KEXEC_LOAD, unix.CAP_SYS_BOOT},
		{unix.SYS_KEXEC_FILE_LOAD, unix.CAP_SYS_BOOT},
		{unix.SYS_REBOOT, unix.CAP_SYS_BOOT},
		{unix.SYS_CLOCK_SETTIME, unix.CAP_SYS_TIME},
		{unix.SYS_CLOCK_ADJTIME, unix.CAP_SYS_TIME},
		{unix.SYS_SETTIMEOFDAY, unix.CAP_SYS_TIME},
		{unix.SYS_ACCT, unix.CAP_SYS_PACCT},
		{unix.SYS_SWAPON, unix.CAP_SYS_ADMIN},
		{unix.SYS_SWAPOFF, unix.CAP_SYS_ADMIN},
		{unix.SYS_QUOTACTL, unix.CAP_SYS_ADMIN},
		{unix.SYS_MBIND, unix.CAP_SYS_NICE},
		{unix.SYS_SET_MEMPOLICY, unix.CAP_SYS_NICE},
		{unix.SYS_MIGRATE_PAGES, unix.CAP_SYS_NICE},
		{unix.SYS_MOVE_PAGES, unix.CAP_SYS_NICE},
		{unix.SYS_MOUNT, unix.CAP_SYS_ADMIN},
		{unix.SYS_UMOUNT2, unix.CAP_SYS_ADMIN},
		{unix.SYS_PIVOT_ROOT, unix.CAP_SYS_ADMIN},
		{unix.SYS_OPEN_TREE, unix.CAP_SYS_ADMIN},
		{unix.SYS_MOVE_MOUNT, unix.CAP_SYS_ADMIN},
		{unix.SYS_FSOPEN, unix.CAP_SYS_ADMIN},
		{unix.SYS_FSCONFIG, unix.CAP_SYS_ADMIN},
		{unix.SYS_FSMOUNT, unix.CAP_SYS_ADMIN},
		{unix.SYS_FSPICK, unix.CAP_SYS_ADMIN},
		{unix.SYS_MOUNT_SETATTR, unix.CAP_SYS_ADMIN},
		{unix.SYS_UNSHARE, unix.CAP_SYS_ADMIN},
		{unix.SYS_SETNS, unix.CAP_SYS_ADMIN},
		{unix.SYS_NAME_TO_HANDLE_AT, unix.CAP_SYS_ADMIN},
		{unix.SYS_OPEN_BY_HANDLE_AT, unix.CAP_SYS_ADMIN},
		{unix.SYS_LOOKUP_DCOOKIE, unix.CAP_SYS_ADMIN},
		{unix.SYS_VHANGUP, unix.CAP_SYS_ADMIN},
		{unix.SYS_BPF, unix.CAP_BPF},
		{unix.SYS_PERF_EVENT_OPEN, unix.CAP_PERFMON},
		{unix.SYS_USERFAULTFD, -1},
	}
}

func restrictSyscalls(unconfined, syncThreads bool, capabilities uint64) error {
	var arch uint32
	switch runtime.GOARCH {
	case "amd64":
		arch = unix.AUDIT_ARCH_X86_64
	case "arm64":
		arch = unix.AUDIT_ARCH_AARCH64
	default:
		return fmt.Errorf("unsupported seccomp architecture")
	}
	load := func(offset uint32) unix.SockFilter {
		return unix.SockFilter{Code: unix.BPF_LD | unix.BPF_W | unix.BPF_ABS, K: offset}
	}
	eq := func(value uint32, yes, no uint8) unix.SockFilter {
		return unix.SockFilter{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, Jt: yes, Jf: no, K: value}
	}
	ret := func(value uint32) unix.SockFilter { return unix.SockFilter{Code: unix.BPF_RET | unix.BPF_K, K: value} }
	deny := ret(unix.SECCOMP_RET_ERRNO | uint32(unix.EPERM))
	filter := []unix.SockFilter{load(4), eq(arch, 1, 0), ret(unix.SECCOMP_RET_KILL_PROCESS), load(0)}
	if runtime.GOARCH == "amd64" {
		// The x32 ABI uses the same audit architecture with different numbers.
		filter = append(filter, unix.SockFilter{Code: unix.BPF_JMP | unix.BPF_JSET | unix.BPF_K, K: 0x40000000, Jf: 1}, deny)
	}
	// The default policy follows Rust Kiri. Each capability relaxes only
	// its corresponding syscalls; vsock and io_uring remain outside every
	// workload's boundary, including privileged nested builders.
	for _, nr := range []uint32{unix.SYS_IO_URING_SETUP, unix.SYS_IO_URING_ENTER, unix.SYS_IO_URING_REGISTER} {
		filter = append(filter, eq(nr, 0, 1), deny)
	}
	if !unconfined {
		for _, rule := range capabilitySyscalls() {
			if rule.capability >= 0 && capabilities&(uint64(1)<<rule.capability) != 0 {
				continue
			}
			filter = append(filter, eq(rule.number, 0, 1), deny)
		}
	}
	filter = append(filter, eq(unix.SYS_SOCKET, 0, 4), load(16), eq(unix.AF_VSOCK, 0, 1),
		ret(unix.SECCOMP_RET_ERRNO|uint32(unix.EAFNOSUPPORT)), load(0))
	if !unconfined && capabilities&(1<<unix.CAP_SYS_ADMIN) == 0 {
		filter = append(filter, eq(unix.SYS_CLONE3, 0, 1), ret(unix.SECCOMP_RET_ERRNO|uint32(unix.ENOSYS)))
		namespaceFlags := uint32(unix.CLONE_NEWNS | unix.CLONE_NEWUTS | unix.CLONE_NEWIPC | unix.CLONE_NEWUSER | unix.CLONE_NEWPID | unix.CLONE_NEWNET | unix.CLONE_NEWCGROUP)
		filter = append(filter, eq(unix.SYS_CLONE, 0, 3), load(16),
			unix.SockFilter{Code: unix.BPF_JMP | unix.BPF_JSET | unix.BPF_K, K: namespaceFlags, Jf: 1}, deny)
	}
	filter = append(filter, ret(unix.SECCOMP_RET_ALLOW))
	program := unix.SockFprog{Len: uint16(len(filter)), Filter: &filter[0]}
	if syncThreads {
		// The init supervisor keeps Go runtime threads alive after startup.
		// They need the same guest transport boundary as the workload.
		tid, _, errno := unix.Syscall(unix.SYS_SECCOMP, unix.SECCOMP_SET_MODE_FILTER, unix.SECCOMP_FILTER_FLAG_TSYNC, uintptr(unsafe.Pointer(&program)))
		runtime.KeepAlive(filter)
		if errno != 0 {
			return fmt.Errorf("install init seccomp filter: %w", errno)
		}
		if tid != 0 {
			return fmt.Errorf("synchronize init seccomp filter with thread %d", tid)
		}
		return nil
	}
	err := unix.Prctl(unix.PR_SET_SECCOMP, unix.SECCOMP_MODE_FILTER, uintptr(unsafe.Pointer(&program)), 0, 0)
	runtime.KeepAlive(filter)
	return err
}
