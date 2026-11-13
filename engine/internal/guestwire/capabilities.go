package guestwire

import (
	"fmt"
	"strings"
)

// CapabilityNone is Rust Kiri's wire marker for an explicit empty set.
const CapabilityNone uint64 = 1 << 63

// AllCapabilities covers the Linux capabilities through CHECKPOINT_RESTORE.
const AllCapabilities uint64 = (1 << 41) - 1

// DefaultCapabilities is Docker and Rust Kiri's default set, in Linux bit order.
const DefaultCapabilities uint64 = 1<<0 | 1<<1 | 1<<3 | 1<<4 | 1<<5 | 1<<6 | 1<<7 | 1<<8 |
	1<<10 | 1<<13 | 1<<18 | 1<<27 | 1<<29 | 1<<31

var capabilityNames = [...]string{
	"CHOWN", "DAC_OVERRIDE", "DAC_READ_SEARCH", "FOWNER", "FSETID", "KILL",
	"SETGID", "SETUID", "SETPCAP", "LINUX_IMMUTABLE", "NET_BIND_SERVICE", "NET_BROADCAST",
	"NET_ADMIN", "NET_RAW", "IPC_LOCK", "IPC_OWNER", "SYS_MODULE", "SYS_RAWIO",
	"SYS_CHROOT", "SYS_PTRACE", "SYS_PACCT", "SYS_ADMIN", "SYS_BOOT", "SYS_NICE",
	"SYS_RESOURCE", "SYS_TIME", "SYS_TTY_CONFIG", "MKNOD", "LEASE", "AUDIT_WRITE",
	"AUDIT_CONTROL", "SETFCAP", "MAC_OVERRIDE", "MAC_ADMIN", "SYSLOG", "WAKE_ALARM",
	"BLOCK_SUSPEND", "AUDIT_READ", "PERFMON", "BPF", "CHECKPOINT_RESTORE",
}

// CapabilityMask applies drops before additions, matching the original engine.
// Zero means defaults; CapabilityNone represents dropping every capability.
func CapabilityMask(add, drop []string) (uint64, error) {
	if len(add)+len(drop) == 0 {
		return 0, nil
	}
	mask := DefaultCapabilities
	bit := func(name string) (uint64, error) {
		n := strings.TrimPrefix(strings.ToUpper(strings.TrimSpace(name)), "CAP_")
		if n == "ALL" {
			return AllCapabilities, nil
		}
		for i, c := range capabilityNames {
			if c == n {
				return uint64(1) << i, nil
			}
		}
		return 0, fmt.Errorf("unknown capability %q", name)
	}
	for _, n := range drop {
		b, e := bit(n)
		if e != nil {
			return 0, e
		}
		mask &^= b
	}
	for _, n := range add {
		b, e := bit(n)
		if e != nil {
			return 0, e
		}
		mask |= b
	}
	return EncodeCapabilities(mask), nil
}

// EncodeCapabilities distinguishes an empty set from the default marker.
func EncodeCapabilities(mask uint64) uint64 {
	if mask == 0 {
		return CapabilityNone
	}
	return mask
}

// EffectiveCapabilities resolves the default marker to the requested mask.
func EffectiveCapabilities(mask uint64) uint64 {
	if mask == 0 {
		return DefaultCapabilities
	}
	return mask & AllCapabilities
}

// ValidateCapabilities rejects wire bits unknown to both engines.
func ValidateCapabilities(mask uint64) error {
	if mask != CapabilityNone && mask & ^AllCapabilities != 0 {
		return fmt.Errorf("invalid capability mask")
	}
	return nil
}
