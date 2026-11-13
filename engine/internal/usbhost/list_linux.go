package usbhost

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

func List() ([]Device, error) { return ListSysfs("/sys/bus/usb/devices") }
func ListSysfs(root string) ([]Device, error) {
	entries, e := os.ReadDir(root)
	if os.IsNotExist(e) {
		return []Device{}, nil
	}
	if e != nil {
		return nil, e
	}
	out := []Device{}
	for _, entry := range entries {
		base := filepath.Join(root, entry.Name())
		read := func(s string) string {
			b, _ := os.ReadFile(filepath.Join(base, s))
			return strings.TrimSpace(string(b))
		}
		if read("idVendor") == "" || read("bDeviceClass") == "09" {
			continue
		}
		vid, _ := strconv.ParseUint(read("idVendor"), 16, 16)
		pid, _ := strconv.ParseUint(read("idProduct"), 16, 16)
		bus, _ := strconv.Atoi(read("busnum"))
		dev, _ := strconv.Atoi(read("devnum"))
		name := strings.TrimSpace(read("manufacturer") + " " + read("product"))
		if name == "" {
			name = "USB device"
		}
		d := Device{Location: entry.Name(), Name: name, Vendor: uint16(vid), Product: uint16(pid), Path: fmt.Sprintf("/dev/bus/usb/%03d/%03d", bus, dev)}
		real, e := filepath.EvalSymlinks(base)
		if e != nil {
			continue
		}
		_ = filepath.WalkDir(real, func(path string, ent os.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			rel, _ := filepath.Rel(real, path)
			if strings.Count(rel, string(filepath.Separator)) > 7 {
				return filepath.SkipDir
			}
			if strings.HasPrefix(ent.Name(), "ttyUSB") || strings.HasPrefix(ent.Name(), "ttyACM") {
				candidate := "/dev/" + ent.Name()
				if d.TTY == "" || candidate < d.TTY {
					d.TTY = candidate
				}
			}
			return nil
		})
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}
