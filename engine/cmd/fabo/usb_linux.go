package main

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/OpenFabrica/Fabo/engine/internal/usbhost"
)

// usbListing is the machine's devices for the window: containers see them
// already, so there is nothing to attach.
func usbListing() usbList {
	devices, e := usbhost.List()
	if e != nil {
		return usbList{Can: "none", Error: e.Error()}
	}
	return usbList{Can: "none", Devices: linuxUSBRows(devices)}
}

// linuxUSBRows are devices as the window lists them: keyed by device node,
// with the serial port where there is one, as `--device` takes it.
func linuxUSBRows(devices []usbhost.Device) []usbRow {
	rows := make([]usbRow, 0, len(devices))
	for _, d := range devices {
		path := d.Path
		if d.TTY != "" {
			path = d.TTY
		}
		rows = append(rows, usbRow{BusID: d.Path, Name: d.Name, ID: d.ID(), Kind: d.Kind, Path: path})
	}
	return rows
}

// usbPrint prints this machine's devices, which containers already see.
func usbPrint() error {
	devices, e := usbhost.List()
	if e != nil {
		return e
	}
	version, _ := os.ReadFile("/proc/version")
	wsl := strings.Contains(strings.ToLower(string(version)), "microsoft")
	if len(devices) == 0 {
		fmt.Println("No USB devices.")
	}
	fmt.Printf("%-22s %-10s %-14s NAME\n", "DEVICE", "ID", "TTY")
	for _, d := range devices {
		tty := d.TTY
		if tty == "" {
			tty = "-"
		}
		fmt.Printf("%-22s %-10s %-14s %s\n", d.Path, d.ID(), tty, d.Name)
	}
	fmt.Println("Containers already see these devices: fabo run --device /dev/ttyUSB0 …")
	if wsl {
		fmt.Println("To add one from Windows: usbipd attach --wsl --busid <BUSID>")
	}
	return nil
}
// usbChange refuses: there is nothing to attach on Linux.
func usbChange(string, string) error {
	return errors.New("nothing to attach here: containers already see this machine's USB devices (--device). Under Windows, attach the device to WSL with usbipd-win first")
}
// usbRemoveHelper refuses: the helper is a macOS thing.
func usbRemoveHelper() error { return errors.New("the USB helper is only used on macOS") }
