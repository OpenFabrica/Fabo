package guestwire

import (
	"fmt"
	"slices"
	"strings"
)

// TmpfsMount creates an ephemeral filesystem with no host source.
type TmpfsMount struct {
	Target  string
	Options []string
}

const tmpfsDefaults = "tmpfs,noexec,nosuid,nodev"

func IsTmpfsMount(wire string) bool { return strings.HasPrefix(wire, "tmpfs\t") }

func (m TmpfsMount) Encode() (string, error) {
	target, err := cleanMountTarget(m.Target)
	if err != nil {
		return "", err
	}
	for _, opt := range m.Options {
		switch opt {
		case "ro", "rw", "exec", "noexec", "suid", "nosuid", "dev", "nodev", "atime", "noatime":
			continue
		}
		key, value, ok := strings.Cut(opt, "=")
		if !ok || !asciiToken(key, "_-") || value == "" || strings.ContainsAny(value, ",=\t\r\n\x00") {
			return "", fmt.Errorf("invalid tmpfs option")
		}
	}
	flags := tmpfsDefaults
	if len(m.Options) != 0 {
		flags += "," + strings.Join(m.Options, ",")
	}
	return "tmpfs\t" + target + "\t" + flags, nil
}

func DecodeTmpfsMount(wire string) (TmpfsMount, error) {
	fields := strings.Split(wire, "\t")
	if len(fields) != 3 || fields[0] != "tmpfs" {
		return TmpfsMount{}, fmt.Errorf("invalid tmpfs mount")
	}
	target, err := cleanMountTarget(fields[1])
	if err != nil {
		return TmpfsMount{}, err
	}
	flags, _ := strings.CutPrefix(fields[2], tmpfsDefaults)
	m := TmpfsMount{Target: target}
	for opt := range strings.SplitSeq(flags, ",") {
		if opt != "" && opt != "tmpfs" {
			m.Options = append(m.Options, opt)
		}
	}
	_, err = m.Encode()
	return m, err
}

func (m TmpfsMount) ReadOnly() bool {
	for _, opt := range slices.Backward(m.Options) {
		if opt == "ro" || opt == "rw" {
			return opt == "ro"
		}
	}
	return false
}
