package guestwire

import (
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
)

// ProcessOptions are the identity and resource settings applied to each
// workload process, independently of the agent's own environment.
type ProcessOptions struct {
	Groups  []string
	Ulimits []Ulimit
}

type Ulimit struct {
	Name       string
	Soft, Hard int64
}

// Linux resource IDs used by the Rust protocol, on both guest architectures.
var limitNames = [...]string{"cpu", "fsize", "data", "stack", "core", "rss", "nproc", "nofile", "memlock", "as", "locks", "sigpending", "msgqueue", "nice", "rtprio", "rttime"}

func (u Ulimit) Values() (resource int, soft, hard uint64, err error) {
	resource = slices.Index(limitNames[:], u.Name)
	if resource < 0 {
		return 0, 0, 0, fmt.Errorf("invalid ulimit type %q", u.Name)
	}
	value := func(n int64) uint64 {
		if n < 0 {
			return math.MaxUint64
		}
		return uint64(n)
	}
	soft, hard = value(u.Soft), value(u.Hard)
	if soft > hard {
		return 0, 0, 0, fmt.Errorf("ulimit %s soft limit exceeds its hard limit", u.Name)
	}
	return resource, soft, hard, nil
}

func (p ProcessOptions) Validate() error {
	for _, g := range p.Groups {
		if g == "" || strings.ContainsAny(g, ":\t\r\n\x00") {
			return fmt.Errorf("invalid supplementary group")
		}
	}
	for _, u := range p.Ulimits {
		if _, _, _, err := u.Values(); err != nil {
			return err
		}
	}
	return nil
}

func (p ProcessOptions) appendOptions(options []string) ([]string, error) {
	if len(p.Groups)+len(p.Ulimits) == 0 {
		return options, nil
	}
	if err := p.Validate(); err != nil {
		return nil, err
	}
	options = slices.Clone(options)
	for _, u := range p.Ulimits {
		res, soft, hard, _ := u.Values()
		options = append(options, fmt.Sprintf("rlimit\t%d\t%d\t%d", res, soft, hard))
	}
	for _, group := range p.Groups {
		options = append(options, "group\t"+group)
	}
	return options, nil
}

func decodeProcessOptions(options []string) (ProcessOptions, []string, error) {
	var p ProcessOptions
	rest := options[:0]
	parseLimit := func(raw string) (int64, error) {
		if raw == "18446744073709551615" {
			return -1, nil
		}
		n, err := strconv.ParseUint(raw, 10, 63)
		return int64(n), err
	}
	for _, line := range options {
		kind, value, _ := strings.Cut(line, "\t")
		switch kind {
		case "group":
			p.Groups = append(p.Groups, value)
		case "rlimit":
			parts := strings.Split(value, "\t")
			if len(parts) != 3 {
				return ProcessOptions{}, nil, fmt.Errorf("invalid guest rlimit")
			}
			res, err := strconv.ParseUint(parts[0], 10, 32)
			if err != nil || res >= uint64(len(limitNames)) {
				return ProcessOptions{}, nil, fmt.Errorf("invalid guest rlimit resource")
			}
			soft, e1 := parseLimit(parts[1])
			hard, e2 := parseLimit(parts[2])
			if e1 != nil || e2 != nil {
				return ProcessOptions{}, nil, fmt.Errorf("invalid guest rlimit value")
			}
			p.Ulimits = append(p.Ulimits, Ulimit{Name: limitNames[res], Soft: soft, Hard: hard})
		default:
			rest = append(rest, line)
		}
	}
	return p, rest, p.Validate()
}
