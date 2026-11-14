//go:build linux || darwin

package main

import (
	"errors"
	"fmt"
	"github.com/OpenFabrica/FabriBay/engine/internal/usbhost"
)

const usbUsage = "usage: fabo usb [list|ls] | attach DEVICE | detach DEVICE | helper remove"

func usbCommand(args []string) error {
	if len(args) > 0 && args[0] == "fake" {
		return usbFake(args[1:])
	}
	if len(args) == 0 || len(args) == 1 && (args[0] == "list" || args[0] == "ls") {
		return usbList()
	}
	if len(args) == 2 && (args[0] == "attach" || args[0] == "detach") && args[1] != "" {
		return usbChange(args[0], args[1])
	}
	if len(args) == 2 && args[0] == "helper" && args[1] == "remove" {
		return usbRemoveHelper()
	}
	return errors.New(usbUsage)
}
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
