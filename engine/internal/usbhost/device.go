package usbhost

import (
	"fmt"
	"strconv"
	"strings"
)

type Device struct {
	Location, Name, Kind, TTY, Path string
	Vendor, Product                 uint16
	Speed, Address                  uint32
}

func (d Device) ID() string { return fmt.Sprintf("%04x:%04x", d.Vendor, d.Product) }
func (d Device) LinuxSpeed() uint32 {
	switch d.Speed {
	case 0:
		return 1
	case 1:
		return 2
	case 2:
		return 3
	default:
		return 5
	}
}
func (d Device) DeviceID() uint32 {
	location, _ := strconv.ParseUint(strings.TrimPrefix(d.Location, "0x"), 16, 32)
	return uint32(location>>24)<<16 | d.Address
}
func (d Device) SpeedName() string {
	names := []string{"low", "full", "high", "super", "super+"}
	if int(d.Speed) >= len(names) {
		return "?"
	}
	return names[d.Speed]
}
func Find(devices []Device, which string) (Device, error) {
	w := strings.ToLower(which)
	location, locErr := strconv.ParseUint(strings.TrimPrefix(w, "0x"), 16, 32)
	found := []Device{}
	for _, d := range devices {
		n, e := strconv.ParseUint(strings.TrimPrefix(d.Location, "0x"), 16, 32)
		if w != "" && (d.ID() == w || d.Location == w || locErr == nil && e == nil && n == location || strings.Contains(strings.ToLower(d.Name), w)) {
			found = append(found, d)
		}
	}
	if len(found) == 1 {
		return found[0], nil
	}
	if len(found) == 0 {
		return Device{}, fmt.Errorf("no USB device matches %q (see fabo usb list)", which)
	}
	return Device{}, fmt.Errorf("%q matches %d devices; use the location", which, len(found))
}
