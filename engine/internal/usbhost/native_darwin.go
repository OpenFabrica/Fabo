//go:build darwin && cgo

package usbhost

/*
#cgo CFLAGS: -x objective-c -fobjc-arc -mmacosx-version-min=14.0
#cgo LDFLAGS: -framework Foundation -framework IOKit -framework IOUSBHost
#include <stdlib.h>
#include "native.h"
*/
import "C"
import (
	"encoding/json"
	"errors"
	"sort"
	"unsafe"
)

func List() ([]Device, error) {
	var message *C.char
	raw := C.fabo_usb_list(&message)
	if message != nil {
		defer C.free(unsafe.Pointer(message))
		return nil, errors.New(C.GoString(message))
	}
	if raw == nil {
		return nil, errors.New("USB enumeration failed")
	}
	defer C.free(unsafe.Pointer(raw))
	var devices []Device
	if e := json.Unmarshal([]byte(C.GoString(raw)), &devices); e != nil {
		return nil, e
	}
	sort.Slice(devices, func(i, j int) bool { return devices[i].Location < devices[j].Location })
	return devices, nil
}

type Captured struct{ handle unsafe.Pointer }

func Capture(location uint32, fd int) (*Captured, error) {
	var message *C.char
	h := C.fabo_usb_capture(C.uint32_t(location), C.int(fd), &message)
	if message != nil {
		defer C.free(unsafe.Pointer(message))
		return nil, errors.New(C.GoString(message))
	}
	if h == nil {
		return nil, errors.New("could not capture USB device")
	}
	return &Captured{h}, nil
}
func (b *Captured) Dispose() { C.fabo_usb_dispose(b.handle) }
func usbBuffer(data []byte) unsafe.Pointer {
	if len(data) == 0 {
		return nil
	}
	return unsafe.Pointer(&data[0])
}
func (b *Captured) Control(_ uint32, s Setup, out []byte) Result {
	data := out
	if s.Type&0x80 != 0 {
		data = make([]byte, s.Length)
	}
	var actual C.size_t
	status := C.fabo_usb_control(b.handle, C.uint8_t(s.Type), C.uint8_t(s.Request), C.uint16_t(s.Value), C.uint16_t(s.Index), C.uint16_t(s.Length), usbBuffer(data), C.size_t(len(data)), &actual)
	n := min(int(actual), len(data))
	if s.Type&0x80 == 0 {
		data = nil
	} else {
		data = data[:n]
	}
	return Result{int32(status), data, n}
}
func (b *Captured) Transfer(_ uint32, endpoint uint8, out []byte, length int) Result {
	data := out
	if endpoint&0x80 != 0 {
		data = make([]byte, length)
	}
	var actual C.size_t
	status := C.fabo_usb_transfer(b.handle, C.uint8_t(endpoint), usbBuffer(data), C.size_t(len(data)), &actual)
	n := min(int(actual), len(data))
	if endpoint&0x80 == 0 {
		data = nil
	} else {
		data = data[:n]
	}
	return Result{int32(status), data, n}
}
func (b *Captured) Configure(v uint8) int32 {
	return int32(C.fabo_usb_configure(b.handle, C.uint8_t(v)))
}
func (b *Captured) Alternate(i, a uint8) int32 {
	return int32(C.fabo_usb_alternate(b.handle, C.uint8_t(i), C.uint8_t(a)))
}
func (b *Captured) ClearHalt(e uint8) int32 { return int32(C.fabo_usb_clear(b.handle, C.uint8_t(e))) }
func (b *Captured) Abort(_ uint32, e uint8) { C.fabo_usb_abort(b.handle, C.uint8_t(e)) }
func (b *Captured) Close()                  { C.fabo_usb_close(b.handle) }
