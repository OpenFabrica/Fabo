package guestwire

import (
	"fmt"
	"path"
	"strings"
)

// RunReadOnlyRoot mounts the container root filesystem read-only.
const RunReadOnlyRoot uint32 = 512

// HostShareTag and HostSharePath identify the VM's VirtioFS mount.
const (
	HostShareTag  = "kiri-mac"
	HostSharePath = "/mnt/kiri-host"
)

type BindMount struct {
	Source, Target string
	ReadOnly       bool
	CopyUp         bool
	Create         bool
	Driver         *VolumeDriverMount
}

// VolumeDriverMount describes the filesystem mounted at a local volume's root.
type VolumeDriverMount struct {
	Type, Device, Options string
}

func (v VolumeDriverMount) Validate() error {
	if v.Type == "" || v.Device == "" || strings.ContainsAny(v.Type+v.Device+v.Options, "\t\n\x00") {
		return fmt.Errorf("local volume requires a filesystem type and device")
	}
	if v.Bind() && !path.IsAbs(v.Device) {
		return fmt.Errorf("local volume bind device must be an absolute path")
	}
	return nil
}

func (v VolumeDriverMount) Bind() bool {
	for option := range strings.SplitSeq(v.Options, ",") {
		if option == "bind" || option == "rbind" {
			return true
		}
	}
	return false
}

func (m BindMount) Encode() (string, error) {
	if !path.IsAbs(m.Source) || !path.IsAbs(m.Target) || strings.ContainsAny(m.Source+m.Target, "\t\x00") {
		return "", fmt.Errorf("bind mount requires absolute source and target paths")
	}
	target, err := cleanMountTarget(m.Target)
	if err != nil {
		return "", err
	}
	mode := "rw"
	if m.ReadOnly {
		mode = "ro"
	}
	if m.Create {
		mode += ",create"
	}
	if m.CopyUp {
		mode += ",copyup"
	}
	value := m.Source + "\t" + target + "\t" + mode
	if m.Driver != nil {
		if err := m.Driver.Validate(); err != nil {
			return "", err
		}
		value += "\t" + m.Driver.Type + "\t" + m.Driver.Device + "\t" + m.Driver.Options
	}
	return value, nil
}

func cleanMountTarget(raw string) (string, error) {
	if !path.IsAbs(raw) || strings.ContainsAny(raw, "\t\x00") {
		return "", fmt.Errorf("mount target must be an absolute path")
	}
	for part := range strings.SplitSeq(raw, "/") {
		if part == ".." {
			return "", fmt.Errorf("invalid bind mount target")
		}
	}
	target := path.Clean(raw)
	for _, reserved := range []string{"/dev", "/proc", "/sys"} {
		if target == reserved || strings.HasPrefix(target, reserved+"/") {
			return "", fmt.Errorf("bind mount target %q is reserved", target)
		}
	}
	if target == "/" {
		return "", fmt.Errorf("mount cannot replace the root filesystem")
	}
	return target, nil
}

func DecodeBindMount(value string) (BindMount, error) {
	fields := strings.Split(value, "\t")
	if len(fields) != 3 && len(fields) != 6 {
		return BindMount{}, fmt.Errorf("unsupported bind mount format")
	}
	m := BindMount{Source: fields[0], Target: fields[1]}
	if len(fields) == 6 {
		m.Driver = &VolumeDriverMount{Type: fields[3], Device: fields[4], Options: fields[5]}
	}
	var mode bool
	for flag := range strings.SplitSeq(fields[2], ",") {
		switch {
		case flag == "":
		case (flag == "ro" || flag == "rw") && !mode:
			m.ReadOnly, mode = flag == "ro", true
		case flag == "create" && !m.Create:
			m.Create = true
		case flag == "copyup" && !m.CopyUp:
			m.CopyUp = true
		default:
			return BindMount{}, fmt.Errorf("unsupported bind mount flag %q", flag)
		}
	}
	if _, err := m.Encode(); err != nil {
		return BindMount{}, err
	}
	m.Target = path.Clean(m.Target)
	return m, nil
}
