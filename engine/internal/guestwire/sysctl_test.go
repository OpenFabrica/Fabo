package guestwire

import (
	"bytes"
	"maps"
	"testing"
)

func TestRunSysctlsUseRustNamespaceFields(t *testing.T) {
	spec := RunSpec{Sysctls: map[string]string{
		"net.core.somaxconn": "256", "net/ipv4/conf/eth0.10/forwarding": "1",
		"kernel.sem": "250 32000 100 128", "kernel.domainname": "fixture",
	}}
	frame, err := spec.Frame()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(frame.Payload, []byte("sysctl\tipc\tkernel/sem\t250 32000 100 128")) ||
		!bytes.Contains(frame.Payload, []byte("sysctl\tuts\tkernel/domainname\tfixture")) {
		t.Fatal("missing Rust sysctl options")
	}
	run, err := DecodeRun(frame)
	if err != nil || !maps.Equal(run.Sysctls, map[string]string{
		"net/core/somaxconn": "256", "net/ipv4/conf/eth0.10/forwarding": "1",
		"kernel/sem": "250 32000 100 128", "kernel/domainname": "fixture",
	}) {
		t.Fatalf("sysctls changed: %+v, %v", run.Sysctls, err)
	}
}

func TestRunSysctlsRequirePrivateNamespaces(t *testing.T) {
	for _, key := range []string{"net/../kernel/domainname", "kernel.core_pattern", "kernel.hostname", "net..core"} {
		if _, err := ParseSysctls(map[string]string{key: "1"}, true); err == nil {
			t.Errorf("accepted %q", key)
		}
	}
	for _, spec := range []RunSpec{
		{Flags: RunNetworkHost, Sysctls: map[string]string{"net.core.somaxconn": "256"}},
		{Network: Network{Options: []string{NetworkJoinPrefix + "abc"}}, Sysctls: map[string]string{"net.core.somaxconn": "256"}},
		{Options: []string{"sysctl\tipc\tkernel/core_pattern\tfixture"}},
		{Options: []string{"sysctl\tuts\tnet/core/somaxconn\t256"}},
	} {
		frame, err := spec.Frame()
		if err == nil {
			_, err = DecodeRun(frame)
		}
		if err == nil {
			t.Errorf("accepted unsafe Run settings %+v", spec)
		}
	}
}
