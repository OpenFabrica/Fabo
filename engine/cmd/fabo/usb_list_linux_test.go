package main

import (
	"testing"

	"github.com/OpenFabrica/Fabo/engine/internal/usbhost"
)

// TestLinuxUSBRows keys devices by node and shows the serial port where
// there is one, as `docker run --device` takes it.
func TestLinuxUSBRows(t *testing.T) {
	rows := linuxUSBRows([]usbhost.Device{
		{Location: "1-2", Name: "CP2102", Vendor: 0x10c4, Product: 0xea60, Path: "/dev/bus/usb/001/004", TTY: "/dev/ttyUSB0"},
		{Location: "1-3", Name: "Keyboard", Vendor: 0x05ac, Product: 0x024f, Path: "/dev/bus/usb/001/005"},
	})
	if len(rows) != 2 || rows[0].BusID != "/dev/bus/usb/001/004" || rows[0].Path != "/dev/ttyUSB0" || rows[1].Path != "/dev/bus/usb/001/005" || rows[0].ID != "10c4:ea60" || rows[0].InFabo {
		t.Fatalf("rows = %+v", rows)
	}
}
