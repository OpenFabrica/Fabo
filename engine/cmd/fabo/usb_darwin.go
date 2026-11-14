package main

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/OpenFabrica/Fabo/engine/internal/usbhost"
)

func (in installation) usbDevices() ([]usbhost.Device, error) {
	engine, e := in.engine()
	if e != nil {
		return nil, e
	}
	cmd := exec.Command(engine, "usb", "list")
	var devices []usbhost.Device
	raw, e := cmd.Output()
	if e != nil {
		if exit, ok := e.(*exec.ExitError); ok {
			return nil, fmt.Errorf("USB enumeration: %s", strings.TrimSpace(string(exit.Stderr)))
		}
		return nil, e
	}
	e = json.Unmarshal(raw, &devices)
	return devices, e
}
func (in installation) usbAttached() (map[string]bool, error) {
	c := newCommandClient(in.socket())
	defer c.http.CloseIdleConnections()
	var entries []struct{ Location string }
	e := c.request("GET", "/fabo/usb", nil, &entries)
	out := map[string]bool{}
	for _, d := range entries {
		out[d.Location] = true
	}
	return out, e
}
func usbList() error {
	in, e := locate()
	if e != nil {
		return e
	}
	devices, e := in.usbDevices()
	if e != nil {
		return e
	}
	attached, _ := in.usbAttached()
	printUSBDevices(devices, attached)
	return nil
}
func usbChange(verb, which string) error {
	in, e := locate()
	if e != nil {
		return e
	}
	devices, e := in.usbDevices()
	if e != nil {
		return e
	}
	device, e := usbhost.Find(devices, which)
	if e != nil {
		return e
	}
	c, e := in.runningClient()
	if e != nil {
		return e
	}
	defer c.http.CloseIdleConnections()
	if verb == "detach" {
		if e := c.request("DELETE", "/fabo/usb/"+device.Location, nil, nil); e != nil {
			return e
		}
		fmt.Printf("%s is back on the Mac.\n", device.Name)
		return nil
	}
	attached, e := in.usbAttached()
	if e != nil {
		return e
	}
	if attached[device.Location] {
		fmt.Printf("%s is already in Fabo.\n", device.Name)
		return nil
	}
	if e = c.request("POST", "/fabo/wake", nil, nil); e != nil {
		return e
	}
	s := newSetup(in)
	helper, e := s.connectUSBHelper()
	if e != nil {
		return e
	}
	defer helper.Close()
	stream, e := usbhost.Connect(in.socket(), device)
	if e != nil {
		return e
	}
	defer stream.Close()
	if e = usbhost.SendToHelper(helper, stream, device.Location); e != nil {
		return e
	}
	fmt.Printf("%s is in Fabo now. In a container: --device /dev/ttyUSB0 (serial), /dev/input (HID) or /dev/bus/usb (raw).\n", device.Name)
	fmt.Printf("It is gone from the Mac until `fabo usb detach %s`.\n", device.Location)
	return nil
}
func (s *setup) usbLabel() string { return strings.Replace(s.in.names.agent, "daemon", "usb", 1) }
func (s *setup) usbPaths() (bin, plist, socket string) {
	label := s.usbLabel()
	return s.root + "/Library/PrivilegedHelperTools/" + label, s.root + "/Library/LaunchDaemons/" + label + ".plist", s.root + "/var/run/" + label + ".sock"
}
func (s *setup) installUSBHelper() error {
	engine, e := s.in.engine()
	if e != nil {
		return e
	}
	raw, e := os.ReadFile(engine)
	if e != nil {
		return e
	}
	sum := fmt.Sprintf("%x", sha256.Sum256(raw))
	bin, plist, _ := s.usbPaths()
	installed, e := os.ReadFile(bin + ".source")
	if e == nil && strings.TrimSpace(string(installed)) == sum && isFile(bin) && isFile(plist) {
		return nil
	}
	label := s.usbLabel()
	escape := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace
	body := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0"><dict>
<key>Label</key><string>%s</string>
<key>ProgramArguments</key><array><string>%s</string><string>usb</string><string>helper</string><string>%s</string><string>%d</string></array>
<key>RunAtLoad</key><true/><key>KeepAlive</key><true/><key>ProcessType</key><string>Interactive</string>
<key>StandardErrorPath</key><string>/var/log/%s.log</string>
</dict></plist>`, label, escape(bin), label, os.Getuid(), label)
	tmp, e := os.CreateTemp("", "fabo-usb-*.plist")
	if e != nil {
		return e
	}
	defer os.Remove(tmp.Name())
	if _, e = tmp.WriteString(body); e != nil {
		tmp.Close()
		return e
	}
	if e = tmp.Close(); e != nil {
		return e
	}
	q := shellQuote
	command := fmt.Sprintf("set -e; mkdir -p %s %s; cp %s %s; /usr/bin/codesign --force --sign - --identifier %s %s; chown root:wheel %s; chmod 755 %s; mv -f %s %s; printf '%%s\\n' %s > %s; chmod 644 %s; cp %s %s; chown root:wheel %s; chmod 644 %s; launchctl bootout system/%s 2>/dev/null || true; launchctl bootstrap system %s", q(filepath.Dir(bin)), q(filepath.Dir(plist)), q(engine), q(bin+".new"), q(label), q(bin+".new"), q(bin+".new"), q(bin+".new"), q(bin+".new"), q(bin), q(sum), q(bin+".source"), q(bin+".source"), q(tmp.Name()), q(plist), q(plist), q(plist), label, q(plist))
	return s.asAdmin(command, "Fabo wants to install its USB helper, so it can connect USB devices to its Linux VM.")
}
func (s *setup) connectUSBHelper() (*net.UnixConn, error) {
	if e := s.installUSBHelper(); e != nil {
		return nil, e
	}
	_, _, path := s.usbPaths()
	var last error
	for n := 0; n < 50; n++ {
		conn, e := net.DialUnix("unix", nil, &net.UnixAddr{Name: path, Net: "unix"})
		if e == nil {
			return conn, nil
		}
		last = e
		time.Sleep(100 * time.Millisecond)
	}
	return nil, fmt.Errorf("the USB helper is not answering: %w", last)
}
func (s *setup) removeUSBHelper() error {
	bin, plist, socket := s.usbPaths()
	if !exists(bin) && !exists(plist) {
		return nil
	}
	if e := s.asAdmin(fmt.Sprintf("launchctl bootout system/%s 2>/dev/null || true; rm -f %s %s %s %s", s.usbLabel(), shellQuote(bin), shellQuote(plist), shellQuote(bin+".source"), shellQuote(socket)), "Fabo wants to remove its USB helper."); e != nil {
		return e
	}
	fmt.Fprintln(s.out, "removed the USB helper")
	return nil
}
func usbRemoveHelper() error {
	in, e := locate()
	if e != nil {
		return e
	}
	if os.Getuid() == 0 {
		return errors.New("run helper remove as the user who installed Fabo")
	}
	return newSetup(in).removeUSBHelper()
}
