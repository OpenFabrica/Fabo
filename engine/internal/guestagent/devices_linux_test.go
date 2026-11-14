package guestagent

import (
	"github.com/OpenFabrica/FabriBay/engine/internal/guestwire"
	"testing"
)

// Interpret the emitted device instructions against actual access requests,
// independently of whether the test host delegates cgroup BPF attachment.
func TestDefaultDeviceProgram(t *testing.T) {
	run := func(p []uint64, kind, access, major, minor uint32) bool {
		ctx := []uint32{access<<16 | kind, major, minor}
		var r [11]uint64
		for pc := 0; pc < len(p); {
			i := p[pc]
			code, dst, src := uint8(i), uint8(i>>8)&15, uint8(i>>12)&15
			off, imm := int(int16(i>>16)), uint32(i>>32)
			pc++
			switch code {
			case 0x61:
				r[dst] = uint64(ctx[off/4])
			case 0xbf:
				r[dst] = r[src]
			case 0x54:
				r[dst] = uint64(uint32(r[dst]) & imm)
			case 0x74:
				r[dst] = uint64(uint32(r[dst]) >> imm)
			case 0xb7:
				r[dst] = uint64(imm)
			case 0x15:
				if r[dst] == uint64(imm) {
					pc += off
				}
			case 0x56:
				if r[dst] != uint64(imm) {
					pc += off
				}
			case 0x95:
				return r[0] == 1
			default:
				t.Fatalf("unknown device opcode %x", code)
			}
		}
		t.Fatal("device program did not return")
		return false
	}
	for _, tt := range []struct {
		k, a, major, minor uint32
		allow              bool
	}{
		{2, 6, 1, 3, true}, {2, 6, 136, 7, true}, {2, 6, 10, 200, true},
		{1, 1, 8, 0, true}, {1, 2, 8, 0, false}, {2, 6, 10, 229, false},
		{2, 2, 1, 1, false}, {2, 6, 1, 9, true},
	} {
		if got := run(deviceProgram(nil), tt.k, tt.a, tt.major, tt.minor); got != tt.allow {
			t.Fatalf("%+v got %v", tt, got)
		}
	}
	readOnly := deviceProgram([]guestwire.DeviceRule{{Kind: 2, Major: 10, Minor: -1, Access: 2}})
	if !run(readOnly, 2, 2, 10, 229) || run(readOnly, 2, 4, 10, 229) || run(readOnly, 1, 2, 10, 229) {
		t.Fatal("custom character-device rule did not grant only the requested access")
	}
}
