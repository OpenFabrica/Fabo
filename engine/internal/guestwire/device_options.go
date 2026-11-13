package guestwire

import (
	"fmt"
	"path"
	"slices"
	"strconv"
	"strings"
)

// DeviceMapping uses Docker's HostConfig names. The source is resolved in
// the Linux environment that runs the container, including on macOS.
type DeviceMapping struct {
	PathOnHost, PathInContainer, CgroupPermissions string
}

type DeviceOptions struct {
	Mappings []DeviceMapping
	Rules    []string
}

// DeviceRule is an allow rule with Linux's cgroup device type and access bits.
// A negative major or minor matches all values for that field.
type DeviceRule struct {
	Kind         uint32
	Major, Minor int64
	Access       uint32
}

func DeviceAccess(mode string) (uint32, error) {
	var access uint32
	if len(mode) == 0 || len(mode) > 3 {
		return 0, fmt.Errorf("invalid device permissions %q", mode)
	}
	for _, ch := range mode {
		switch ch {
		case 'm':
			access |= 1
		case 'r':
			access |= 2
		case 'w':
			access |= 4
		default:
			return 0, fmt.Errorf("invalid device permissions %q", mode)
		}
	}
	return access, nil
}

func (d DeviceMapping) Normalize() (DeviceMapping, error) {
	if d.PathInContainer == "" {
		d.PathInContainer = d.PathOnHost
	}
	if d.CgroupPermissions == "" {
		d.CgroupPermissions = "rwm"
	}
	for _, p := range []string{d.PathOnHost, d.PathInContainer} {
		if !path.IsAbs(p) || strings.ContainsAny(p, "\x00\t\r\n") || slices.Contains(strings.Split(p, "/"), "..") {
			return DeviceMapping{}, fmt.Errorf("invalid device path %q", p)
		}
	}
	d.PathInContainer = path.Clean(d.PathInContainer)
	if d.PathInContainer == "/" {
		return DeviceMapping{}, fmt.Errorf("a device cannot replace the container root")
	}
	if _, err := DeviceAccess(d.CgroupPermissions); err != nil {
		return DeviceMapping{}, err
	}
	return d, nil
}

// ParseDeviceRule accepts the Docker device-cgroup-rule syntax.
func ParseDeviceRule(text string) (DeviceRule, error) {
	fields := strings.Split(text, " ")
	invalid := fmt.Errorf("invalid device cgroup rule %q", text)
	if len(fields) != 3 {
		return DeviceRule{}, invalid
	}
	var kind uint32
	switch fields[0] {
	case "a":
	case "b":
		kind = 1
	case "c":
		kind = 2
	default:
		return DeviceRule{}, invalid
	}
	major, minor, ok := strings.Cut(fields[1], ":")
	if !ok {
		return DeviceRule{}, invalid
	}
	number := func(text string) (int64, error) {
		if text == "*" {
			return -1, nil
		}
		n, err := strconv.ParseUint(text, 10, 32)
		return int64(n), err
	}
	ma, e1 := number(major)
	mi, e2 := number(minor)
	access, e3 := DeviceAccess(fields[2])
	if e1 != nil || e2 != nil || e3 != nil {
		return DeviceRule{}, invalid
	}
	if kind == 0 {
		ma, mi = -1, -1
	}
	return DeviceRule{Kind: kind, Major: ma, Minor: mi, Access: access}, nil
}

func (d DeviceOptions) Empty() bool { return len(d.Mappings) == 0 && len(d.Rules) == 0 }

func (d DeviceOptions) Validate() error {
	for _, mapping := range d.Mappings {
		if _, err := mapping.Normalize(); err != nil {
			return err
		}
	}
	for _, rule := range d.Rules {
		if _, err := ParseDeviceRule(rule); err != nil {
			return err
		}
	}
	return nil
}

func (d DeviceOptions) appendOptions(options []string) ([]string, error) {
	if d.Empty() {
		return options, nil
	}
	if err := d.Validate(); err != nil {
		return nil, err
	}
	options = slices.Clone(options)
	for _, mapping := range d.Mappings {
		m, _ := mapping.Normalize()
		options = append(options, "device\t"+m.PathOnHost+"\t"+m.PathInContainer+"\t"+m.CgroupPermissions)
	}
	for _, rule := range d.Rules {
		options = append(options, "devrule\t"+strings.NewReplacer(" ", "\t", ":", "\t").Replace(rule))
	}
	return options, nil
}

func decodeDeviceOptions(options []string) (DeviceOptions, []string, error) {
	var devices DeviceOptions
	rest := options[:0]
	for _, option := range options {
		fields := strings.Split(option, "\t")
		switch fields[0] {
		case "device":
			if len(fields) != 4 {
				return DeviceOptions{}, nil, fmt.Errorf("invalid guest device mapping")
			}
			m, err := (DeviceMapping{fields[1], fields[2], fields[3]}).Normalize()
			if err != nil {
				return DeviceOptions{}, nil, err
			}
			devices.Mappings = append(devices.Mappings, m)
		case "devrule":
			if len(fields) != 5 {
				return DeviceOptions{}, nil, fmt.Errorf("invalid guest device rule")
			}
			rule := fields[1] + " " + fields[2] + ":" + fields[3] + " " + fields[4]
			if _, err := ParseDeviceRule(rule); err != nil {
				return DeviceOptions{}, nil, err
			}
			devices.Rules = append(devices.Rules, rule)
		default:
			rest = append(rest, option)
		}
	}
	return devices, rest, nil
}
