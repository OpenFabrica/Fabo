package main

import (
	"context"
	"fmt"
	"net"
	"syscall"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"

	"github.com/OpenFabrica/Fabo/engine/internal/domaindns"
	"github.com/OpenFabrica/Fabo/engine/internal/winhost"
)

// nrptRules is where Windows keeps local NRPT rules, one subkey each.
const nrptRules = `SYSTEM\CurrentControlSet\Services\Dnscache\Parameters\DnsPolicyConfig`

// soExclusiveAddrUse is SO_EXCLUSIVEADDRUSE, a macro in winsock2.h
// (~SO_REUSEADDR) that x/sys/windows does not define.
const soExclusiveAddrUse = ^windows.SO_REUSEADDR

// listenDNS binds 127.0.0.1:53 with SO_EXCLUSIVEADDRUSE: Windows otherwise
// lets another program bind the same address later and take the queries.
func (h *host) listenDNS() (net.PacketConn, error) {
	lc := net.ListenConfig{Control: func(_, _ string, c syscall.RawConn) error {
		var opErr error
		if err := c.Control(func(fd uintptr) {
			opErr = windows.SetsockoptInt(windows.Handle(fd), windows.SOL_SOCKET, soExclusiveAddrUse, 1)
		}); err != nil {
			return err
		}
		return opErr
	}}
	return lc.ListenPacket(context.Background(), "udp4", "127.0.0.1:53")
}

// nrptRuleInstalled reports whether a local NRPT rule covers .fabo.local.
// Rules are subkeys with a Name value of their namespaces (REG_MULTI_SZ,
// though a plain string is read too).
func (h *host) nrptRuleInstalled() bool {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, nrptRules, registry.ENUMERATE_SUB_KEYS)
	if err != nil {
		return false
	}
	defer k.Close()
	subkeys, _ := k.ReadSubKeyNames(-1)
	for _, name := range subkeys {
		rule, err := registry.OpenKey(k, name, registry.QUERY_VALUE)
		if err != nil {
			continue
		}
		names, _, err := rule.GetStringsValue("Name")
		if err != nil {
			if one, _, err := rule.GetStringValue("Name"); err == nil {
				names = []string{one}
			}
		}
		rule.Close()
		if nrptHasNamespace(names) {
			return true
		}
	}
	return false
}

// installNRPTRule adds (or removes) the rule by fabo-admin.ps1 -Dns, run
// as administrator after Windows asks. A script file from the install
// folder checked against this build's SHA-256, never script text passed
// encoded (see winhost.PowerShellArgs). The script removes any rule for
// the namespace before adding one, so a failure leaves none rather than half.
func (h *host) installNRPTRule(on bool) error {
	script, err := h.verified("fabo-admin.ps1")
	if err != nil {
		return err
	}
	args := []string{"-Dns", "-Suffix", domaindns.Suffix}
	if !on {
		args = append(args, "-Off")
	}
	code, err := winhost.ElevatedPowerShell(script, args...)
	if err == winhost.ErrDeclined {
		return errCanceled
	}
	if err != nil {
		h.logf("dns: the elevated PowerShell did not start (%v)", err)
		return err
	}
	if code != 0 {
		return fmt.Errorf("the DNS rule for %s could not be %s (exit code %d)", nrptNamespace, map[bool]string{true: "added", false: "removed"}[on], code)
	}
	return nil
}
