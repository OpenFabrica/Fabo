package main

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/OpenFabrica/Fabo/engine/internal/usbhost"
)

func usbList() error {
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
func usbChange(string, string) error {
	return errors.New("nothing to attach here: containers already see this machine's USB devices (--device). Under Windows, attach the device to WSL with usbipd-win first")
}
func usbRemoveHelper() error { return errors.New("the USB helper is only used on macOS") }
