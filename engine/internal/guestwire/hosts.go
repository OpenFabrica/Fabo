package guestwire

import (
	"fmt"
	"net/netip"
	"strings"
	"unicode"
	"unicode/utf8"
)

// HostGateway is resolved by the guest for the selected network.
const HostGateway = "host-gateway"

const domainnameOption = "domainname="

// ValidateDomainname accepts a Linux UTS domain that can also appear in hosts.
func ValidateDomainname(name string) error {
	if len(name) > 64 || !utf8.ValidString(name) || strings.IndexFunc(name, func(r rune) bool {
		return r == '#' || unicode.IsSpace(r) || unicode.IsControl(r)
	}) >= 0 {
		return fmt.Errorf("invalid domain name %q", name)
	}
	return nil
}

// ParseExtraHost parses an address and hostname from a Run extra-host entry.
// It accepts the host-gateway token and returns canonical IP text otherwise.
func ParseExtraHost(entry string) (address, hostname string, err error) {
	address, names, err := ParseHostsLine(entry)
	if err != nil {
		return "", "", err
	}
	if len(names) != 1 {
		return "", "", fmt.Errorf("invalid extra host entry")
	}
	return address, names[0], nil
}

// ParseHostsLine also accepts the multiple names in a legacy link entry.
func ParseHostsLine(entry string) (address string, names []string, err error) {
	fields := strings.Fields(entry)
	if len(fields) < 2 || strings.ContainsAny(entry, "\r\n") {
		return "", nil, fmt.Errorf("invalid extra host entry")
	}
	address, names = fields[0], fields[1:]
	for _, hostname := range names {
		for _, ch := range hostname {
			if !((ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9') || ch == '-' || ch == '_' || ch == '.') {
				return "", nil, fmt.Errorf("invalid extra hostname")
			}
		}
	}
	if address == HostGateway {
		return address, names, nil
	}
	if inner, ok := strings.CutPrefix(address, "["); ok {
		address, ok = strings.CutSuffix(inner, "]")
		if !ok {
			return "", nil, fmt.Errorf("invalid bracketed extra host address")
		}
	}
	ip, err := netip.ParseAddr(address)
	if err != nil || ip.Zone() != "" {
		return "", nil, fmt.Errorf("invalid extra host address")
	}
	return ip.String(), names, nil
}
