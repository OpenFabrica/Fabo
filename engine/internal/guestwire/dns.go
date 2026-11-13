package guestwire

import (
	"fmt"
	"net/netip"
	"strings"
)

// DNSConfig describes a container's resolver settings independently of its
// endpoints. The Run frame carries these as Rust-compatible network options.
type DNSConfig struct {
	Servers []netip.Addr
	Search  []string
	Options []string
}

func (d DNSConfig) Empty() bool {
	return len(d.Servers)+len(d.Search)+len(d.Options) == 0
}

func (d DNSConfig) Validate() error {
	for _, addr := range d.Servers {
		if !addr.IsValid() || addr.Zone() != "" {
			return fmt.Errorf("invalid DNS server address")
		}
	}
	for _, name := range d.Search {
		if !asciiToken(name, ".-_") || len(name) > 254 {
			return fmt.Errorf("invalid DNS search domain")
		}
	}
	for _, option := range d.Options {
		if !asciiToken(option, ":.-_") {
			return fmt.Errorf("invalid DNS resolver option")
		}
	}
	return nil
}

func asciiToken(s, symbols string) bool {
	for _, ch := range s {
		if !(ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || strings.ContainsRune(symbols, ch)) {
			return false
		}
	}
	return s != ""
}

func (d DNSConfig) appendOptions(options []string) []string {
	for _, addr := range d.Servers {
		options = append(options, "nameserver="+addr.String())
	}
	for _, name := range d.Search {
		options = append(options, "search="+name)
	}
	for _, opt := range d.Options {
		options = append(options, "option="+opt)
	}
	return options
}

func decodeDNSOptions(options []string) (DNSConfig, []string, error) {
	var dns DNSConfig
	remaining := options[:0]
	for _, opt := range options {
		key, value, _ := strings.Cut(opt, "=")
		switch key {
		case "nameserver":
			addr, err := netip.ParseAddr(value)
			if err != nil {
				return DNSConfig{}, nil, fmt.Errorf("invalid DNS server address")
			}
			dns.Servers = append(dns.Servers, addr)
		case "search":
			dns.Search = append(dns.Search, value)
		case "option":
			dns.Options = append(dns.Options, value)
		default:
			remaining = append(remaining, opt)
		}
	}
	return dns, remaining, dns.Validate()
}
