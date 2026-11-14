package usbhost

import (
	"encoding/binary"
	"net"
	"slices"
	"sync"
	"time"
	"unicode/utf16"
)

// Fake is a software CDC ACM serial port or HID mouse for exercising the
// same USB/IP path as a captured device, without hardware or privileges.
type Fake struct {
	Mouse           bool
	UnplugAfter     int
	StallOnce       bool
	Reports         int
	Conn            net.Conn
	halted          bool
	mu              sync.Mutex
	ready           *sync.Cond
	echo, coding    []byte
	echoed, cleared int
	aborted         map[uint32]bool
	closed          bool
}

func NewFake(mouse bool, reports int) *Fake {
	f := &Fake{Mouse: mouse, Reports: reports, aborted: map[uint32]bool{}}
	f.ready = sync.NewCond(&f.mu)
	return f
}

var mouseReport = []byte{5, 1, 9, 2, 0xa1, 1, 9, 1, 0xa1, 0, 5, 9, 0x19, 1, 0x29, 3, 0x15, 0, 0x25, 1, 0x95, 3, 0x75, 1, 0x81, 2, 0x95, 1, 0x75, 5, 0x81, 3, 5, 1, 9, 0x30, 9, 0x31, 9, 0x38, 0x15, 0x81, 0x25, 0x7f, 0x75, 8, 0x95, 3, 0x81, 6, 0xc0, 0xc0}

func (f *Fake) descriptor(kind, index uint8) []byte {
	switch kind {
	case 1:
		class, product := byte(2), byte(1)
		if f.Mouse {
			class, product = 0, 2
		}
		return []byte{18, 1, 0, 2, class, 0, 0, 64, 9, 0x12, product, 0, 0, 1, 1, 2, 3, 1}
	case 2:
		body := []byte{9, 4, 0, 0, 1, 2, 2, 1, 0, 5, 0x24, 0, 0x10, 1, 5, 0x24, 1, 0, 1, 4, 0x24, 2, 2, 5, 0x24, 6, 0, 1, 7, 5, 0x81, 3, 8, 0, 16, 9, 4, 1, 0, 2, 0x0a, 0, 0, 0, 7, 5, 0x82, 2, 64, 0, 0, 7, 5, 2, 2, 64, 0, 0}
		interfaces := byte(2)
		if f.Mouse {
			interfaces = 1
			body = []byte{9, 4, 0, 0, 1, 3, 1, 2, 0, 9, 0x21, 0x11, 1, 0, 1, 0x22, byte(len(mouseReport)), 0, 7, 5, 0x81, 3, 4, 0, 10}
		}
		return append([]byte{9, 2, byte(9 + len(body)), 0, interfaces, 1, 0, 0x80, 50}, body...)
	case 3:
		if index == 0 {
			return []byte{4, 3, 9, 4}
		}
		names := []string{"", "Fabo", "Fabo test serial", "FABO0001"}
		if f.Mouse {
			names[2] = "Fabo test mouse"
		}
		if int(index) >= len(names) {
			return nil
		}
		data := []byte{0, 3}
		for _, u := range utf16.Encode([]rune(names[index])) {
			data = binary.LittleEndian.AppendUint16(data, u)
		}
		data[0] = byte(len(data))
		return data
	}
	return nil
}
func (f *Fake) Control(_ uint32, s Setup, out []byte) Result {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case s.Type == 0x80 && s.Request == 6:
		if data := f.descriptor(uint8(s.Value>>8), uint8(s.Value)); data != nil {
			return Result{Data: data}
		}
	case f.Mouse && s.Type == 0x81 && s.Request == 6 && s.Value>>8 == 0x22:
		return Result{Data: slices.Clone(mouseReport)}
	case (s.Type == 0x80 || s.Type == 0x81 || s.Type == 0x82) && s.Request == 0:
		return Result{Data: []byte{0, 0}}
	case s.Type == 0x80 && s.Request == 8:
		return Result{Data: []byte{1}}
	case f.Mouse && s.Type == 0x21 && (s.Request == 0x0a || s.Request == 0x0b):
		return Result{}
	case !f.Mouse && s.Type == 0x21 && s.Request == 0x20:
		f.coding = slices.Clone(out)
		return Result{Actual: len(out)}
	case !f.Mouse && s.Type == 0xa1 && s.Request == 0x21:
		data := slices.Clone(f.coding)
		if len(data) != 7 {
			data = []byte{0, 0xc2, 1, 0, 0, 0, 8}
		}
		return Result{Data: data}
	case !f.Mouse && s.Type == 0x21 && (s.Request == 0x22 || s.Request == 0x23):
		return Result{}
	}
	return Result{Status: -32}
}
func (f *Fake) Configure(value uint8) int32 {
	if value > 1 {
		return -22
	}
	return 0
}
func (f *Fake) Alternate(_ uint8, alt uint8) int32 {
	if alt != 0 {
		return -22
	}
	return 0
}
func (f *Fake) ClearHalt(_ uint8) int32 {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cleared++
	f.halted = false
	f.ready.Broadcast()
	return 0
}
func (f *Fake) Transfer(seq uint32, ep uint8, out []byte, length int) Result {
	if f.Mouse && ep == 0x81 {
		time.Sleep(10 * time.Millisecond)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.Mouse && ep == 2 {
		if f.closed {
			return Result{Status: -108}
		}
		f.echo = append(f.echo, out...)
		f.echoed += len(out)
		f.ready.Broadcast()
		if f.UnplugAfter > 0 && f.echoed >= f.UnplugAfter {
			if f.Conn != nil {
				f.Conn.Close()
			}
			return Result{Status: -19}
		}
		return Result{Actual: len(out)}
	}
	if !f.Mouse && ep == 0x82 && f.StallOnce {
		f.StallOnce = false
		f.halted = true
		return Result{Status: -32}
	}
	if ep != 0x81 && (f.Mouse || ep != 0x82) {
		return Result{Status: -32}
	}
	for {
		if f.closed {
			return Result{Status: -108}
		}
		if f.aborted[seq] {
			delete(f.aborted, seq)
			return Result{Status: -104}
		}
		if f.Mouse && f.Reports > 0 {
			f.Reports--
			return Result{Data: []byte{0, 1, 0, 0}}
		}
		if !f.Mouse && ep == 0x82 && f.halted {
			return Result{Status: -32}
		}
		if !f.Mouse && ep == 0x82 && len(f.echo) > 0 {
			n := min(length, len(f.echo))
			data := slices.Clone(f.echo[:n])
			f.echo = f.echo[n:]
			return Result{Data: data}
		}
		f.ready.Wait()
	}
}
func (f *Fake) Abort(seq uint32, _ uint8) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.aborted[seq] = true
	f.ready.Broadcast()
}
func (f *Fake) Close()            { f.mu.Lock(); defer f.mu.Unlock(); f.closed = true; f.ready.Broadcast() }
func (f *Fake) Stats() (int, int) { f.mu.Lock(); defer f.mu.Unlock(); return f.echoed, f.cleared }
