//go:build linux || darwin

package localengine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/OpenFabrica/Fabo/engine/internal/guestwire"
)

type kiriContainer struct {
	ID, Name, Status string
	Spec             kiriContainerSpec
	Layers           []string
	CreatedNS        int64  `json:"created_ns"`
	StartedNS        *int64 `json:"started_ns"`
	FinishedNS       *int64 `json:"finished_ns"`
	VMStopped        bool   `json:"vm_stopped"`
	ExitCode         int32  `json:"exit_code"`
	OOMKilled        bool   `json:"oom_killed"`
	StoppedByUser    bool   `json:"stopped_by_user"`
	RestartCount     int    `json:"restart_count"`
}

type kiriContainerSpec struct {
	Image, Cwd, User, Hostname, Domainname string
	ImageID                                string `json:"image_id"`
	NetworkMode                            string `json:"network_mode"`
	Entrypoint, Cmd, Env, Shell            []string
	Labels                                 map[string]string
	TTY, Privileged                        bool
	OpenStdin                              bool      `json:"open_stdin"`
	AutoRemove                             bool      `json:"auto_remove"`
	ReadonlyRoot                           bool      `json:"readonly_rootfs"`
	StopSignal                             uint32    `json:"stop_signal"`
	StopTimeout                            *int64    `json:"stop_timeout"`
	ConsoleSize                            [2]uint16 `json:"console_size"`
	Healthcheck                            *healthConfig
	Restart                                struct {
		Name       string
		MaxRetries int `json:"max_retries"`
	}
	Limits struct {
		MemoryMax uint64 `json:"memory_max"`
		CPUQuota  uint64 `json:"cpu_quota"`
		CPUPeriod uint32 `json:"cpu_period"`
		CPUWeight uint32 `json:"cpu_weight"`
		PIDsMax   uint32 `json:"pids_max"`
		ShmSize   uint64 `json:"shm_size"`
	}
	Ports []struct {
		ContainerPort uint16 `json:"container_port"`
		HostPort      uint16 `json:"host_port"`
		Protocol      string `json:"proto"`
		HostIP        string `json:"host_ip"`
	}
	Exposed   []uint16
	Mounts    []string
	MountInfo []struct {
		Type, Name, Source, Destination string
		RW                              bool
	} `json:"mount_info"`
	Volumes       []string
	AnonVolumes   []string `json:"anon_volumes"`
	Aliases       []string
	IPv4Address   string `json:"ipv4_address"`
	IPv6Address   string `json:"ipv6_address"`
	MACAddress    string `json:"mac_address"`
	ExtraNetworks []struct {
		Name        string
		Aliases     []string
		IPv4Address string `json:"ipv4_address"`
		IPv6Address string `json:"ipv6_address"`
		MACAddress  string `json:"mac_address"`
	} `json:"extra_networks"`
	ExtraHosts  []string `json:"extra_hosts"`
	Caps        uint64
	Init        *bool
	CapAdd      []string `json:"cap_add"`
	CapDrop     []string `json:"cap_drop"`
	SecurityOpt []string `json:"security_opt"`
	DNS         []netip.Addr
	Links       []containerLink
	DNSSearch   []string                   `json:"dns_search"`
	DNSOptions  []string                   `json:"dns_options"`
	HostOpts    map[string]json.RawMessage `json:"host_opts"`
	LogConfig   struct {
		Type   string
		Config map[string]string
	} `json:"log_config"`
}

type migrationContainer struct {
	container  *container
	mode       string
	endpoints  map[string]endpointConfig
	extraHosts []string
}

func (e *Engine) migrateKiriContainers(ctx context.Context, source *os.Root, destination string) error {
	records, err := readKiriIndex[kiriContainer](source, "containers.json")
	if err != nil {
		return err
	}
	slices.SortFunc(records, func(a, b kiriContainer) int { return strings.Compare(a.ID, b.ID) })
	migrated := make([]migrationContainer, 0, len(records))
	names := make(map[string]bool)
	for _, old := range records {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !validBlobName(old.ID) || !containerNamePattern.MatchString(old.Name) || names[old.Name] || e.containers[old.ID] != nil {
			return fmt.Errorf("invalid or duplicate source container %q", old.Name)
		}
		names[old.Name] = true
		newRecord, err := e.convertKiriContainer(old, destination)
		if err != nil {
			return fmt.Errorf("container %q: %w", old.Name, err)
		}
		if err := readKiriLogs(ctx, source, newRecord.container); err != nil {
			return fmt.Errorf("container %q logs: %w", old.Name, err)
		}
		migrated = append(migrated, newRecord)
	}
	if err := e.importBridgeIPv6(source.Name(), migrated); err != nil {
		return err
	}
	// Reserve fixed addresses independently for each family. Allocating a
	// dynamic IPv6 address here for an IPv4-only reservation could take a
	// later container's fixed IPv6 address.
	for _, record := range migrated {
		c := record.container
		reserved := &container{}
		var endpoints []containerEndpoint
		for name, config := range record.endpoints {
			if !config.ip.IsValid() && !config.ip6.IsValid() {
				continue
			}
			n, err := e.network(name)
			if err != nil {
				return err
			}
			network := guestwire.Network{Bridge: n.Bridge}
			if config.ip.IsValid() {
				network, err = e.allocateNetwork(n, config.ip)
				if err != nil {
					return fmt.Errorf("container %q: %w", c.name, err)
				}
			}
			if config.ip6.IsValid() {
				ip, err := e.allocateIPv6(n, config.ip6)
				if err != nil {
					return fmt.Errorf("container %q: %w", c.name, err)
				}
				network.SetIPv6(netip.PrefixFrom(ip, n.IPv6.Subnet.Bits()), n.IPv6.Gateway)
			}
			endpoints = append(endpoints, containerEndpoint{Name: n.Name, ID: n.ID, Network: network})
		}
		reserved.setEndpoints(endpoints)
		e.containers[c.id] = reserved
	}
	for _, record := range migrated {
		if err := ctx.Err(); err != nil {
			return err
		}
		c := record.container
		delete(e.containers, c.id)
		endpoints, err := e.initialEndpoints(record.mode, record.endpoints, nil, c.name, c.id)
		if err != nil {
			return fmt.Errorf("container %q: %w", c.name, err)
		}
		c.networkName = record.mode
		c.setEndpoints(endpoints)
		for _, entry := range record.extraHosts {
			address, name, err := guestwire.ParseExtraHost(entry)
			if err != nil {
				return fmt.Errorf("container %q: %w", c.name, err)
			}
			if address == guestwire.HostRelayAddress {
				address = guestwire.HostGateway
			}
			c.run.Network.ExtraHosts = append(c.run.Network.ExtraHosts, address+"\t"+name)
		}
		if _, err := c.run.Frame(); err != nil {
			return fmt.Errorf("container %q exceeds guest protocol limits: %w", c.name, err)
		}
		if err := e.save(c); err != nil {
			return err
		}
		e.containers[c.id] = c
	}
	return nil
}

func (e *Engine) convertKiriContainer(old kiriContainer, destination string) (migrationContainer, error) {
	spec := old.Spec
	if spec.AutoRemove {
		return migrationContainer{}, fmt.Errorf("cannot migrate an auto-remove container")
	}
	for _, link := range spec.Links {
		if !validBlobName(link.ID) || link.ID == old.ID || !containerNamePattern.MatchString(link.Name) || !containerNamePattern.MatchString(link.Alias) {
			return migrationContainer{}, fmt.Errorf("invalid saved container link")
		}
	}
	security, err := securityFlags(spec.SecurityOpt)
	if err != nil {
		return migrationContainer{}, err
	}
	capabilities := capabilityConfig{Add: spec.CapAdd, Drop: spec.CapDrop}
	caps, err := capabilities.mask()
	if err != nil {
		return migrationContainer{}, err
	}
	if caps != spec.Caps {
		return migrationContainer{}, fmt.Errorf("inconsistent capability settings")
	}
	process, err := parseProcessOptions(spec.HostOpts)
	if err != nil {
		return migrationContainer{}, err
	}
	sysctls, err := parseSysctlConfig(spec.HostOpts)
	if err != nil {
		return migrationContainer{}, err
	}
	devices, err := parseDeviceOptions(spec.HostOpts)
	if err != nil {
		return migrationContainer{}, err
	}
	for _, raw := range spec.HostOpts {
		if hasKiriValue(raw) {
			return migrationContainer{}, fmt.Errorf("uses unsupported host configuration")
		}
	}
	logging, err := newLogConfig(spec.LogConfig.Type, spec.LogConfig.Config)
	if err != nil {
		return migrationContainer{}, fmt.Errorf("uses an unsupported log driver or settings: %w", err)
	}
	// Running records indicate an unsafe or inconsistent snapshot. The
	// source shutdown must finalize exits before we copy the host state.
	if old.Status != "created" && old.Status != "exited" {
		return migrationContainer{}, fmt.Errorf("source record is %q; stop the original workloads before taking the snapshot", old.Status)
	}
	base, err := e.findImage(spec.ImageID)
	if err != nil || base.ID != spec.ImageID {
		return migrationContainer{}, fmt.Errorf("original image %s is missing", spec.ImageID)
	}
	layerOrder := make([]string, 0, len(base.Layers))
	for _, layer := range slices.Backward(base.Layers) {
		layerOrder = append(layerOrder, layer.Digest)
	}
	if !slices.Equal(layerOrder, old.Layers) {
		return migrationContainer{}, fmt.Errorf("container's layer list does not match its image")
	}
	cmd := append(slices.Clone(spec.Entrypoint), spec.Cmd...)
	if len(cmd) == 0 || cmd[0] == "" || spec.StopTimeout != nil && !validTimeout(*spec.StopTimeout) {
		return migrationContainer{}, fmt.Errorf("invalid saved command or stop timeout")
	}
	restart := restartPolicy{Name: spec.Restart.Name, MaximumRetryCount: spec.Restart.MaxRetries}
	encoded, err := json.Marshal(restart)
	if err != nil {
		return migrationContainer{}, err
	}
	if _, err := parseRestartPolicy(encoded); err != nil {
		return migrationContainer{}, err
	}
	run := guestwire.RunSpec{ID: old.ID, Argv: cmd, Env: spec.Env, Cwd: spec.Cwd, User: spec.User, Hostname: spec.Hostname, Domainname: spec.Domainname, Layers: layerOrder}
	run.Flags = security
	run.Caps = caps
	run.Process = process
	run.DNS = guestwire.DNSConfig{Servers: spec.DNS, Search: spec.DNSSearch, Options: spec.DNSOptions}
	if err := run.DNS.Validate(); err != nil {
		return migrationContainer{}, err
	}
	if run.Hostname == "" {
		run.Hostname = old.ID[:12]
	}
	if spec.TTY {
		run.Flags |= guestwire.RunTTY
	}
	if spec.OpenStdin {
		run.Flags |= guestwire.RunStdin
	}
	if spec.Privileged {
		run.Flags |= guestwire.RunPrivileged
	}
	if spec.ReadonlyRoot {
		run.Flags |= guestwire.RunReadOnlyRoot
	}
	run.Limits = guestwire.Limits(spec.Limits)
	mode := spec.NetworkMode
	if mode == "" || mode == "default" {
		mode = "bridge"
	}
	if _, err := guestwire.ParseSysctls(sysctls, mode != "host" && !strings.HasPrefix(mode, containerNetworkPrefix)); err != nil {
		return migrationContainer{}, err
	}
	run.Sysctls, run.Devices = sysctls, devices
	if strings.HasPrefix(mode, containerNetworkPrefix) {
		// Kiri used the target's files even for records with DNS options.
		run.DNS = guestwire.DNSConfig{}
	}
	endpoints, err := kiriEndpoints(spec, mode)
	if err != nil {
		return migrationContainer{}, err
	}
	mounts, volumes, err := e.kiriMounts(spec, destination)
	if err != nil {
		return migrationContainer{}, err
	}
	run.Mounts = mounts
	bindings := make([]portBinding, 0, len(spec.Ports))
	for _, port := range spec.Ports {
		ip, err := parseBindHost(port.HostIP)
		if err != nil || port.ContainerPort == 0 || (port.Protocol != "tcp" && port.Protocol != "udp") || mode == "none" || strings.HasPrefix(mode, containerNetworkPrefix) {
			return migrationContainer{}, fmt.Errorf("invalid saved port binding")
		}
		bindings = append(bindings, portBinding{port.ContainerPort, port.Protocol, ip, port.HostPort})
	}
	explicit := make(map[string]struct{}, len(spec.Exposed))
	for _, port := range spec.Exposed {
		explicit[(portBinding{ContainerPort: port, Protocol: "tcp"}).key()] = struct{}{}
	}
	exposed, bindings := mergePorts(base.ExposedPorts, explicit, bindings, false)
	c := &container{
		id: old.ID, name: old.Name, image: spec.Image, imageID: base.ID, run: run,
		created: time.Unix(0, old.CreatedNS).Unix(), state: old.Status, exit: old.ExitCode, oomKilled: old.OOMKilled,
		entrypoint: append([]string{}, spec.Entrypoint...), blobs: slices.Clone(base.Layers),
		portBindings: bindings, exposedPorts: exposed, volumeNames: volumes, labels: spec.Labels,
		anonymousVolumes: slices.Clone(spec.AnonVolumes),
		stopSignal:       spec.StopSignal, stopTimeout: 10, consoleSize: spec.ConsoleSize, logConfig: logging,
		healthcheck: spec.Healthcheck, shell: spec.Shell, restartPolicy: restart, restartCount: old.RestartCount,
		manualStopped: old.StoppedByUser,
		vmStopped:     old.VMStopped,
		init:          spec.Init, capabilities: capabilities, securityOpt: spec.SecurityOpt, links: spec.Links,
	}
	if old.StartedNS != nil {
		c.startedAt = time.Unix(0, *old.StartedNS).UTC()
	}
	if old.FinishedNS != nil {
		c.finishedAt = time.Unix(0, *old.FinishedNS).UTC()
	}
	if spec.StopTimeout != nil {
		c.stopTimeout = *spec.StopTimeout
	}
	return migrationContainer{c, mode, endpoints, spec.ExtraHosts}, nil
}

func kiriEndpoints(spec kiriContainerSpec, mode string) (map[string]endpointConfig, error) {
	if strings.HasPrefix(mode, containerNetworkPrefix) {
		if spec.IPv4Address != "" || spec.IPv6Address != "" || spec.MACAddress != "" || len(spec.ExtraNetworks)+len(spec.Aliases)+len(spec.ExtraHosts) != 0 {
			return nil, fmt.Errorf("shared container network has conflicting saved options")
		}
		return nil, nil
	}
	endpoints := make(map[string]endpointConfig)
	add := func(name, address, ip6, mac string, aliases []string) error {
		var config endpointConfig
		if mac != "" {
			address, err := guestwire.ParseEndpointMAC(mac)
			if err != nil {
				return fmt.Errorf("network %q: %w", name, err)
			}
			config.mac = address
		}
		if address != "" {
			ip, err := netip.ParseAddr(address)
			if err != nil || !ip.Is4() {
				return fmt.Errorf("invalid fixed IPv4 address on %q", name)
			}
			config.ip = ip
		}
		if ip6 != "" {
			ip, err := netip.ParseAddr(ip6)
			if err != nil || !ip.Is6() || ip.Is4In6() || ip.Zone() != "" {
				return fmt.Errorf("invalid fixed IPv6 address on %q", name)
			}
			config.ip6 = ip
		}
		for _, alias := range aliases {
			if !validNetworkAlias(alias) {
				return fmt.Errorf("invalid network alias on %q", name)
			}
		}
		config.aliases = aliases
		if _, duplicate := endpoints[name]; duplicate {
			return fmt.Errorf("duplicate network %q", name)
		}
		endpoints[name] = config
		return nil
	}
	if err := add(mode, spec.IPv4Address, spec.IPv6Address, spec.MACAddress, spec.Aliases); err != nil {
		return nil, err
	}
	for _, ep := range spec.ExtraNetworks {
		if err := add(ep.Name, ep.IPv4Address, ep.IPv6Address, ep.MACAddress, ep.Aliases); err != nil {
			return nil, err
		}
	}
	return endpoints, nil
}

func (e *Engine) kiriMounts(spec kiriContainerSpec, destination string) ([]string, []string, error) {
	if len(spec.Mounts) != len(spec.MountInfo) {
		return nil, nil, fmt.Errorf("incomplete mount metadata")
	}
	var mounts, volumes []string
	for i, entry := range spec.MountInfo {
		fields := strings.Split(spec.Mounts[i], "\t")
		if (len(fields) != 3 && !(entry.Type == "volume" && len(fields) == 7)) || fields[1] != entry.Destination {
			return nil, nil, fmt.Errorf("unsupported mount at %q", entry.Destination)
		}
		if entry.Type == "tmpfs" {
			mount, err := guestwire.DecodeTmpfsMount(spec.Mounts[i])
			if err != nil {
				return nil, nil, fmt.Errorf("tmpfs at %q: %w", entry.Destination, err)
			}
			value, err := mount.Encode()
			if err != nil {
				return nil, nil, err
			}
			mounts = append(mounts, value)
			continue
		}
		flags := strings.Split(fields[2], ",")
		for _, flag := range flags {
			switch flag {
			case "", "ro", "create", "copyup":
			default:
				return nil, nil, fmt.Errorf("unsupported option on mount at %q", entry.Destination)
			}
		}
		m := guestwire.BindMount{Source: entry.Source, Target: entry.Destination, ReadOnly: !entry.RW}
		switch entry.Type {
		case "volume":
			v, err := e.readVolume(entry.Name)
			if err != nil {
				return nil, nil, err
			}
			m.Driver, err = localVolumeMount(v.Options)
			if err != nil {
				return nil, nil, err
			}
			if len(fields) == 7 && m.Driver == nil {
				return nil, nil, fmt.Errorf("volume %q has no driver options for its saved mount", entry.Name)
			}
			if !slices.Contains(volumes, entry.Name) {
				volumes = append(volumes, entry.Name)
			}
			if e.remoteGuest {
				m.Source, m.Create = guestwire.VMVolumePath(entry.Name), true
			} else {
				m.Source = filepath.Join(destination, "volumes", entry.Name, "_data")
			}
			m.CopyUp = slices.Contains(flags, "copyup")
		case "bind":
			if e.remoteGuest {
				if fields[0] == guestwire.EngineSocketPath {
					m.Source = guestwire.EngineSocketPath
					break
				}
				if fields[0] != "/mnt/mac"+entry.Source {
					return nil, nil, fmt.Errorf("mount at %q is not backed by the host share", entry.Destination)
				}
				if _, err := e.guestShares.source(m.Source, false); err != nil {
					// A bind whose host file is gone, as a /tmp file is after
					// a restart, keeps its container, as Docker keeps one:
					// starting it fails and says why. It must still lie
					// inside the VM shares.
					if _, statErr := os.Lstat(m.Source); !errors.Is(statErr, os.ErrNotExist) {
						return nil, nil, err
					}
					if err := e.guestShares.shared(m.Source); err != nil {
						return nil, nil, err
					}
				}
			} else {
				if fields[0] == e.engineSocket && (engineSocketAlias(entry.Source) || entry.Source == e.engineSocket) {
					// The new daemon may use a different socket path.
					m.Source = "/var/run/docker.sock"
				} else if (fields[0] != entry.Source && fields[0] != "/mnt/mac"+entry.Source) || slices.Contains(flags, "create") {
					return nil, nil, fmt.Errorf("unsupported host bind at %q", entry.Destination)
				}
			}
		default:
			return nil, nil, fmt.Errorf("unsupported mount type %q", entry.Type)
		}
		wire, err := m.Encode()
		if err != nil {
			return nil, nil, err
		}
		mounts = append(mounts, wire)
	}
	for _, name := range append(slices.Clone(spec.Volumes), spec.AnonVolumes...) {
		if !slices.Contains(volumes, name) {
			return nil, nil, fmt.Errorf("missing mount for volume %q", name)
		}
	}
	return mounts, volumes, nil
}
