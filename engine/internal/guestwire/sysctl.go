package guestwire

import (
	"fmt"
	"maps"
	"slices"
	"strings"
)

type Sysctl struct {
	Namespace, Path, Value string
}

// ParseSysctls normalizes Docker's dot and slash forms and confines settings
// to the container's own kernel namespaces.
func ParseSysctls(values map[string]string, privateNetwork bool) ([]Sysctl, error) {
	var settings []Sysctl
	paths := make(map[string]bool, len(values))
	for _, key := range slices.Sorted(maps.Keys(values)) {
		namespace, path, err := sysctlPath(key)
		if err != nil {
			return nil, err
		}
		if namespace == "net" && !privateNetwork {
			return nil, fmt.Errorf("sysctl %q requires a private network namespace", key)
		}
		if paths[path] {
			return nil, fmt.Errorf("duplicate sysctl %q", key)
		}
		value := values[key]
		if strings.ContainsAny(value, "\t\r\n\x00") {
			return nil, fmt.Errorf("invalid value for sysctl %q", key)
		}
		settings = append(settings, Sysctl{namespace, path, value})
		paths[path] = true
	}
	return settings, nil
}

func sysctlPath(key string) (namespace, path string, err error) {
	path = key
	if !strings.Contains(path, "/") {
		path = strings.ReplaceAll(path, ".", "/")
	}
	for part := range strings.SplitSeq(path, "/") {
		if part == "." || part == ".." || !asciiToken(part, "._-:@+") {
			return "", "", fmt.Errorf("invalid sysctl %q", key)
		}
	}
	switch path {
	case "kernel/hostname":
		return "", "", fmt.Errorf("sysctl %q conflicts with the container hostname", key)
	case "kernel/domainname":
		return "uts", path, nil
	case "kernel/msgmax", "kernel/msgmnb", "kernel/msgmni", "kernel/sem", "kernel/shmall", "kernel/shmmax", "kernel/shmmni", "kernel/shm_rmid_forced":
		return "ipc", path, nil
	}
	switch {
	case strings.HasPrefix(path, "fs/mqueue/"):
		return "ipc", path, nil
	case strings.HasPrefix(path, "net/"):
		return "net", path, nil
	default:
		return "", "", fmt.Errorf("sysctl %q is not namespaced", key)
	}
}

// PrivateNetwork reports whether Run creates a new network namespace.
func (s RunSpec) PrivateNetwork() bool {
	return s.Flags&RunNetworkHost == 0 && !slices.ContainsFunc(s.Network.Options, func(opt string) bool {
		return strings.HasPrefix(opt, NetworkJoinPrefix)
	})
}

func (s RunSpec) appendSysctlOptions(options []string) ([]string, error) {
	if len(s.Sysctls) == 0 {
		return options, nil
	}
	settings, err := ParseSysctls(s.Sysctls, s.PrivateNetwork())
	if err != nil {
		return nil, err
	}
	options = slices.Clone(options)
	for _, setting := range settings {
		options = append(options, "sysctl\t"+setting.Namespace+"\t"+setting.Path+"\t"+setting.Value)
	}
	return options, nil
}

func decodeSysctlOptions(options []string, privateNetwork bool) (map[string]string, []string, error) {
	var values map[string]string
	rest := options[:0]
	for _, line := range options {
		value, ok := strings.CutPrefix(line, "sysctl\t")
		if !ok {
			rest = append(rest, line)
			continue
		}
		fields := strings.Split(value, "\t")
		if len(fields) != 3 {
			return nil, nil, fmt.Errorf("invalid guest sysctl option")
		}
		namespace, path, err := sysctlPath(fields[1])
		if err != nil || fields[0] != namespace || fields[1] != path {
			return nil, nil, fmt.Errorf("invalid guest sysctl namespace or path")
		}
		if values == nil {
			values = make(map[string]string)
		}
		if _, exists := values[path]; exists {
			return nil, nil, fmt.Errorf("duplicate guest sysctl %q", path)
		}
		values[path] = fields[2]
	}
	_, err := ParseSysctls(values, privateNetwork)
	return values, rest, err
}
