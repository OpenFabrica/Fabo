package guestagent

import (
	"fmt"
	"os"
	"runtime"
	"unsafe"

	"golang.org/x/sys/unix"

	"github.com/OpenFabrica/FabriBay/engine/internal/guestwire"
)

// deviceProgram permits the original default devices and any explicit rules.
func deviceProgram(extra []guestwire.DeviceRule) []uint64 {
	const mknod = unix.BPF_DEVCG_ACC_MKNOD
	const fullAccess = mknod | unix.BPF_DEVCG_ACC_READ | unix.BPF_DEVCG_ACC_WRITE
	const character = unix.BPF_DEVCG_DEV_CHAR
	rules := []guestwire.DeviceRule{
		{Major: -1, Minor: -1, Access: mknod},
		{Kind: character, Major: 136, Minor: -1, Access: fullAccess},
	}
	for _, minor := range []int64{3, 5, 7, 8, 9} {
		rules = append(rules, guestwire.DeviceRule{Kind: character, Major: 1, Minor: minor, Access: fullAccess})
	}
	for _, minor := range []int64{0, 1, 2} {
		rules = append(rules, guestwire.DeviceRule{Kind: character, Major: 5, Minor: minor, Access: fullAccess})
	}
	rules = append(rules, guestwire.DeviceRule{Kind: character, Major: 10, Minor: 200, Access: fullAccess})
	rules = append(rules, extra...)
	insn := func(code, dst, src uint8, off int16, imm int32) uint64 {
		return uint64(code) | uint64(src<<4|dst)<<8 | uint64(uint16(off))<<16 | uint64(uint32(imm))<<32
	}
	p := []uint64{
		insn(0x61, 2, 1, 0, 0), // r2 = ctx.access_type
		insn(0xbf, 3, 2, 0, 0),
		insn(0x54, 3, 0, 0, 0xffff), // r3 = device type
		insn(0x74, 2, 0, 0, 16),     // r2 = requested access bits
		insn(0x61, 4, 1, 4, 0),      // r4 = ctx.major
		insn(0x61, 5, 1, 8, 0),      // r5 = ctx.minor
	}
	for _, r := range rules {
		start := len(p)
		if r.Kind != 0 {
			p = append(p, insn(0x56, 3, 0, 0, int32(r.Kind)))
		}
		if r.Major >= 0 {
			p = append(p, insn(0x56, 4, 0, 0, int32(r.Major)))
		}
		if r.Minor >= 0 {
			p = append(p, insn(0x56, 5, 0, 0, int32(r.Minor)))
		}
		comparisons := len(p)
		p = append(p, insn(0xbf, 6, 2, 0, 0), insn(0x54, 6, 0, 0, int32(^r.Access&0xffff)))
		// On a mismatch, skip this rule's local allow result.
		p = append(p, insn(0x56, 6, 0, 2, 0), insn(0xb7, 0, 0, 0, 1), insn(0x95, 0, 0, 0, 0))
		for i := start; i < comparisons; i++ {
			p[i] |= uint64(len(p)-i-1) << 16
		}
	}
	return append(p, insn(0xb7, 0, 0, 0, 0), insn(0x95, 0, 0, 0, 0))
}

func restrictDevices(cgroup *os.File, rules []guestwire.DeviceRule) error {
	if cgroup == nil {
		return fmt.Errorf("no delegated cgroup")
	}
	program := deviceProgram(rules)
	license := []byte("GPL\x00")
	// Linux bpf_attr for BPF_PROG_LOAD, through prog_name; absent tail is zero.
	load := struct {
		Type, Count          uint32
		Instructions         *uint64
		License              *byte
		LogLevel, LogSize    uint32
		LogBuffer            uint64
		KernelVersion, Flags uint32
		Name                 [16]byte
	}{Type: unix.BPF_PROG_TYPE_CGROUP_DEVICE, Count: uint32(len(program)), Instructions: &program[0], License: &license[0]}
	fd, _, errno := unix.Syscall(unix.SYS_BPF, unix.BPF_PROG_LOAD, uintptr(unsafe.Pointer(&load)), unsafe.Sizeof(load))
	runtime.KeepAlive(program)
	runtime.KeepAlive(license)
	if errno != 0 {
		return fmt.Errorf("load cgroup device filter: %w", errno)
	}
	defer unix.Close(int(fd))
	attach := struct{ Target, Program, Type, Flags uint32 }{uint32(cgroup.Fd()), uint32(fd), unix.BPF_CGROUP_DEVICE, unix.BPF_F_ALLOW_MULTI}
	_, _, errno = unix.Syscall(unix.SYS_BPF, unix.BPF_PROG_ATTACH, uintptr(unsafe.Pointer(&attach)), unsafe.Sizeof(attach))
	runtime.KeepAlive(cgroup)
	if errno != 0 {
		return fmt.Errorf("attach cgroup device filter: %w", errno)
	}
	return nil
}
