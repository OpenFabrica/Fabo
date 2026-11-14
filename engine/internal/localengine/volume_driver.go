//go:build linux || darwin

package localengine

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/OpenFabrica/FabriBay/engine/internal/guestwire"
)

func localVolumeMount(options map[string]string) (*guestwire.VolumeDriverMount, error) {
	if len(options) == 0 {
		return nil, nil
	}
	for key := range options {
		switch key {
		case "type", "device", "o":
		case "size":
			return nil, fmt.Errorf("%w: local volume quota is unavailable", errInvalid)
		default:
			return nil, fmt.Errorf("%w: unsupported local driver option %q", errInvalid, key)
		}
	}
	m := &guestwire.VolumeDriverMount{Type: options["type"], Device: options["device"], Options: options["o"]}
	if err := m.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %v", errInvalid, err)
	}
	if m.Type == "cifs" {
		if _, _, err := net.SplitHostPort(cifsVolumeHost(m.Device)); err == nil {
			return nil, fmt.Errorf("%w: specify a CIFS port with the port mount option", errInvalid)
		}
	}
	return m, nil
}

func cifsVolumeHost(device string) string {
	unc, ok := strings.CutPrefix(device, "//")
	if !ok {
		return ""
	}
	host, _, _ := strings.Cut(unc, "/")
	return host
}

// Resolve network mounts on the host at start time. Container records retain
// the original options, including hostnames, for the next start or migration.
func resolveVolumeMounts(ctx context.Context, mounts []string) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	for i, value := range mounts {
		if guestwire.IsTmpfsMount(value) {
			continue
		}
		m, err := guestwire.DecodeBindMount(value)
		if err != nil {
			return err
		}
		v := m.Driver
		if v == nil || v.Bind() {
			continue
		}
		switch v.Type {
		case "nfs", "nfs4":
			options := strings.Split(v.Options, ",")
			for j, option := range options {
				if host, ok := strings.CutPrefix(option, "addr="); ok {
					ip, err := resolveVolumeHost(ctx, host)
					if err != nil {
						return err
					}
					options[j] = "addr=" + ip
				}
			}
			v.Options = strings.Join(options, ",")
		case "cifs":
			host := cifsVolumeHost(v.Device)
			if host == "" || net.ParseIP(strings.Trim(host, "[]")) != nil {
				continue
			}
			ip, err := resolveVolumeHost(ctx, host)
			if err != nil {
				return err
			}
			v.Device = "//" + ip + strings.TrimPrefix(v.Device, "//"+host)
		default:
			continue
		}
		mounts[i], err = m.Encode()
		if err != nil {
			return err
		}
	}
	return nil
}

func resolveVolumeHost(ctx context.Context, host string) (string, error) {
	if ip := net.ParseIP(host); ip != nil {
		return ip.String(), nil
	}
	addresses, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil || len(addresses) == 0 {
		return "", fmt.Errorf("resolve local volume server %q", host)
	}
	for _, address := range addresses {
		if address.IP.To4() != nil {
			return address.IP.String(), nil
		}
	}
	return addresses[0].String(), nil
}
