//go:build linux || darwin

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/OpenFabrica/Fabo/engine/internal/usbhost"
)

const usbUsage = "usage: fabo usb [list|ls] [--json] | attach DEVICE | detach DEVICE | helper remove"

// usbCommand dispatches `fabo usb`.
func usbCommand(args []string) error {
	if len(args) > 0 && args[0] == "fake" {
		return usbFake(args[1:])
	}
	if len(args) == 2 && (args[0] == "list" || args[0] == "ls") && args[1] == "--json" {
		return printUSBListing(usbListing())
	}
	if len(args) == 0 || len(args) == 1 && (args[0] == "list" || args[0] == "ls") {
		return usbPrint()
	}
	if len(args) == 2 && (args[0] == "attach" || args[0] == "detach") && args[1] != "" {
		return usbChange(args[0], args[1])
	}
	if len(args) == 2 && args[0] == "helper" && args[1] == "remove" {
		return usbRemoveHelper()
	}
	return errors.New(usbUsage)
}
// usbRow is one device of `fabo usb list --json`, the window's USB page
// (desktop/usb.go reads the same shape from fabo-win on Windows). BusID is
// the key attach and detach take: the location on a Mac, the device node
// on Linux.
type usbRow struct {
	BusID  string `json:"busid"`
	Name   string `json:"name"`
	ID     string `json:"id"`
	Kind   string `json:"kind,omitempty"`
	Speed  string `json:"speed,omitempty"`
	Path   string `json:"path,omitempty"`
	InFabo bool   `json:"inFabo"`
	Since  int64  `json:"since,omitempty"`
}

// usbList is `fabo usb list --json`. Can is "attach" where devices move
// into Fabo (macOS) and "none" where containers already see them (Linux).
// A failed enumeration is Error, not an exit status, so the window can
// show it.
type usbList struct {
	Can     string   `json:"can"`
	Error   string   `json:"error,omitempty"`
	Devices []usbRow `json:"devices"`
}

// printUSBListing writes the listing as one line of JSON.
func printUSBListing(l usbList) error {
	if l.Devices == nil {
		l.Devices = []usbRow{}
	}
	return json.NewEncoder(os.Stdout).Encode(l)
}

// printUSBDevices is `fabo usb list` on a Mac, as a table.
func printUSBDevices(devices []usbhost.Device, attached map[string]bool) {
	if len(devices) == 0 {
		fmt.Println("No USB devices.")
		return
	}
	fmt.Printf("%-12s %-10s %-6s %-10s %-9s NAME\n", "LOCATION", "ID", "SPEED", "STATE", "KIND")
	for _, d := range devices {
		state := "on the Mac"
		if attached[d.Location] {
			state = "in Fabo"
		}
		kind := d.Kind
		if kind == "" {
			kind = "usb"
		}
		fmt.Printf("%-12s %-10s %-6s %-10s %-9s %s\n", d.Location, d.ID(), d.SpeedName(), state, kind, d.Name)
	}
}
