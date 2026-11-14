package guestagent

import (
	"errors"
	"fmt"
	"os"
	"slices"

	"golang.org/x/sys/unix"

	"github.com/OpenFabrica/Fabo/engine/internal/guestwire"
)

var networkSysctlDefaults = [...]guestwire.Sysctl{
	{Namespace: "net", Path: "net/ipv4/ip_unprivileged_port_start", Value: "0"},
	{Namespace: "net", Path: "net/ipv4/ping_group_range", Value: "0 2147483647"},
}

// The bootstrap runs in the child's own namespaces. The parent has already
// installed the network interfaces by the time it releases the child.
func applySysctls(values map[string]string, privateNetwork bool) error {
	settings, err := guestwire.ParseSysctls(values, privateNetwork)
	if err != nil {
		return err
	}
	if privateNetwork {
		for _, setting := range networkSysctlDefaults {
			if !slices.ContainsFunc(settings, func(current guestwire.Sysctl) bool { return current.Path == setting.Path }) {
				if err := writeSysctl(setting); err != nil {
					return err
				}
			}
		}
	}
	for _, setting := range settings {
		if err := writeSysctl(setting); err != nil {
			return err
		}
	}
	return nil
}

func writeSysctl(setting guestwire.Sysctl) error {
	f, err := os.OpenFile("/proc/sys/"+setting.Path, os.O_WRONLY|unix.O_NOFOLLOW, 0)
	if err == nil {
		_, err = f.WriteString(setting.Value)
		err = errors.Join(err, f.Close())
	}
	if err != nil {
		return fmt.Errorf("write sysctl %s: %w", setting.Path, err)
	}
	return nil
}

func protectProcSys() error {
	const path = "/proc/sys"
	if err := unix.Mount(path, path, "", unix.MS_BIND, ""); err != nil {
		return err
	}
	return unix.MountSetattr(unix.AT_FDCWD, path, 0, &unix.MountAttr{Attr_set: unix.MOUNT_ATTR_RDONLY})
}
