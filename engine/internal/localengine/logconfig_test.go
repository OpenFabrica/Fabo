//go:build linux || darwin

package localengine

import (
	"strings"
	"testing"
)

// json-file's max-size and max-file are read as Kiri reads them, and their
// product bounds the kept output below the engine's own limit.
func TestLogConfigFollowsKiri(t *testing.T) {
	for options, want := range map[string]int{
		"":                          containerLogLimit,
		"max-size=50m":              containerLogLimit,
		"max-size=100k":             3 * 100 << 10,
		"max-size=1kb,max-file=5":   5 << 10,
		"max-size=2KiB,max-file=1":  2 << 10,
		"max-size=-1":               containerLogLimit,
		"max-file=2000,max-size=1k": 1000 << 10,
	} {
		config := map[string]string{}
		for _, pair := range strings.Split(options, ",") {
			if k, v, ok := strings.Cut(pair, "="); ok {
				config[k] = v
			}
		}
		l, err := newLogConfig("json-file", config)
		if err != nil {
			t.Fatalf("%q: %v", options, err)
		}
		if got, _ := l.limit(); got != want {
			t.Errorf("%q: limit %d, want %d", options, got, want)
		}
	}
	for kind, config := range map[string]map[string]string{
		"syslog":    nil,
		"json-file": {"compress": "true"},
		"":          {"max-file": "0"},
		"local":     {"max-size": "1m"},
	} {
		if _, err := newLogConfig(kind, config); err == nil {
			t.Errorf("%q %v was accepted", kind, config)
		}
	}
	if _, err := newLogConfig("", map[string]string{"max-size": "ten"}); err == nil || !strings.Contains(err.Error(), "invalid size: 'ten'") {
		t.Errorf("max-size=ten: %v", err)
	}
}

// A container keeps no more output than its own limit.
func TestContainerLogsStayWithinItsLimit(t *testing.T) {
	l, err := newLogConfig("json-file", map[string]string{"max-size": "1k", "max-file": "2"})
	if err != nil {
		t.Fatal(err)
	}
	c := &container{logConfig: l}
	for range 10 {
		c.appendLog(record{data: []byte(strings.Repeat("x", 500))})
	}
	if c.logBytes > 2<<10 || c.logBytes < 1500 {
		t.Fatalf("kept %d bytes, want at most 2048", c.logBytes)
	}
}
