package guestagent

import (
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/OpenFabrica/FabriBay/engine/internal/guestwire"
)

// networkFiles keeps the guest's generated files available for the lifetime
// of the namespace's users. Reference changes are serialized by Runtime.mu.
type networkFiles struct {
	dir   string
	binds []guestwire.BindMount
	refs  int
}

func prepareNetworkFiles(spec guestwire.RunSpec, vm bool, dir string) (*networkFiles, error) {
	hosts, err := containerHosts(spec, vm)
	if err != nil {
		return nil, err
	}
	type networkFile struct {
		name, content string
		readOnly      bool
	}
	files := []networkFile{{"hosts", hosts, false}}
	if spec.Hostname != "" {
		files = append(files, networkFile{"hostname", spec.Hostname + "\n", false})
	}
	content, err := containerResolver(spec, vm)
	if err != nil {
		return nil, err
	}
	if content != nil {
		files = append(files, networkFile{"resolv.conf", string(content), true})
	}
	staging, err := os.MkdirTemp(dir, ".network-")
	if err != nil {
		return nil, err
	}
	result := &networkFiles{dir: staging, refs: 1}
	for _, file := range files {
		source := filepath.Join(staging, file.name)
		if err := os.WriteFile(source, []byte(file.content), 0644); err != nil {
			result.release()
			return nil, err
		}
		result.binds = append(result.binds, guestwire.BindMount{Source: source, Target: "/etc/" + file.name, ReadOnly: file.readOnly})
	}
	return result, nil
}

func containerDNSAddress(spec guestwire.RunSpec, vm bool) netip.Addr {
	if spec.Flags&guestwire.RunNetworkHost != 0 {
		return netip.Addr{}
	}
	custom := guestwire.CustomBridge(spec.Network.Bridge) ||
		slices.ContainsFunc(spec.AdditionalNetworks, func(n guestwire.Network) bool { return guestwire.CustomBridge(n.Bridge) })
	if custom {
		return loopbackResolver
	}
	if len(spec.DNS.Servers) != 0 {
		return netip.Addr{}
	}
	if vm && spec.Network.Bridge == guestwire.DefaultBridge && spec.Network.Gateway != [4]byte{} {
		return netip.AddrFrom4(spec.Network.Gateway)
	}
	return loopbackResolver
}

func containerResolver(spec guestwire.RunSpec, vm bool) ([]byte, error) {
	var base []byte
	if spec.Flags&guestwire.RunNetworkHost != 0 {
		var err error
		base, err = os.ReadFile("/etc/resolv.conf")
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	}
	return resolverContent(base, spec, vm), nil
}

func resolverContent(base []byte, spec guestwire.RunSpec, vm bool) []byte {
	dns := spec.DNS
	address := containerDNSAddress(spec, vm)
	if !address.IsValid() && dns.Empty() {
		return base
	}
	replaceServers := address.IsValid() || len(dns.Servers) != 0
	var result strings.Builder
	for line := range strings.SplitSeq(string(base), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		key := fields[0]
		if replaceServers && key == "nameserver" ||
			len(dns.Search) != 0 && (key == "search" || key == "domain") ||
			len(dns.Options) != 0 && key == "options" {
			continue
		}
		result.WriteString(line + "\n")
	}
	if address.IsValid() {
		fmt.Fprintf(&result, "nameserver %s\n", address)
	} else {
		for _, addr := range dns.Servers {
			fmt.Fprintf(&result, "nameserver %s\n", addr)
		}
	}
	search := slices.DeleteFunc(slices.Clone(dns.Search), func(name string) bool { return name == "." })
	if len(search) > 0 {
		fmt.Fprintf(&result, "search %s\n", strings.Join(search, " "))
	}
	if len(dns.Options) > 0 {
		fmt.Fprintf(&result, "options %s\n", strings.Join(dns.Options, " "))
	}
	return []byte(result.String())
}

func (f *networkFiles) release() {
	f.refs--
	if f.refs == 0 {
		_ = os.RemoveAll(f.dir)
	}
}

// mount shares the managed files unless the container has explicit mounts.
func (f *networkFiles) mount(spec *guestwire.RunSpec) error {
	mounted, err := networkMountTargets(spec.Mounts)
	if err != nil || mounted["/etc"] {
		return err
	}
	mounts := slices.Clone(spec.Mounts)
	for _, bind := range f.binds {
		if mounted[bind.Target] {
			continue
		}
		value, err := bind.Encode()
		if err != nil {
			return err
		}
		mounts = append(mounts, value)
	}
	spec.Mounts = mounts
	return nil
}

func networkMountTargets(mounts []string) (map[string]bool, error) {
	mounted := make(map[string]bool, len(mounts))
	for _, value := range mounts {
		if guestwire.IsTmpfsMount(value) {
			m, err := guestwire.DecodeTmpfsMount(value)
			if err != nil {
				return nil, err
			}
			mounted[m.Target] = true
			continue
		}
		m, err := guestwire.DecodeBindMount(value)
		if err != nil {
			return nil, err
		}
		mounted[m.Target] = true
	}
	return mounted, nil
}

func containerHosts(spec guestwire.RunSpec, vm bool) (string, error) {
	var hosts strings.Builder
	hosts.WriteString("127.0.0.1\tlocalhost\n::1\tlocalhost ip6-localhost ip6-loopback\n")
	if name := spec.Hostname; name != "" && !strings.ContainsAny(name, " \t\r\n#") {
		address := "127.0.1.1"
		if spec.Network.IP != [4]byte{} {
			address = netip.AddrFrom4(spec.Network.IP).String()
		}
		names := name
		if spec.Domainname != "" {
			names = name + "." + spec.Domainname + " " + name
		}
		fmt.Fprintf(&hosts, "%s\t%s\n", address, names)
		if ip, _, _ := spec.Network.IPv6(); ip.IsValid() && !guestwire.IPv6Disabled(spec.Sysctls, "eth0") {
			fmt.Fprintf(&hosts, "%s\t%s\n", ip.Addr(), names)
		}
	}
	gateway := guestwire.HostRelayAddress
	if !vm && spec.Network.Gateway != [4]byte{} {
		gateway = netip.AddrFrom4(spec.Network.Gateway).String()
	}
	for _, entry := range spec.Network.ExtraHosts {
		address, names, err := guestwire.ParseHostsLine(entry)
		if err != nil {
			return "", err
		}
		if address == guestwire.HostGateway {
			address = gateway
		}
		fmt.Fprintf(&hosts, "%s\t%s\n", address, strings.Join(names, " "))
	}
	if vm && spec.Flags&guestwire.RunNetworkHost != 0 {
		fmt.Fprintf(&hosts, "%s\t%s\n", guestwire.HostRelayAddress, strings.Join(hostRelayNames, " "))
	}
	return hosts.String(), nil
}
