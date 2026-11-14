package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// asUser stands in for the password prompt: the sandbox's machine paths are
// the test's own, so the command runs as it is.
func asUser(command, _ string) error {
	return exec.Command("/bin/sh", "-c", command).Run()
}

// The resolver rule is written once, read back as the names being on, and
// removed again; turning on what is on asks for nothing.
func TestLocalNamesInstallAndRemoveTheResolverRule(t *testing.T) {
	s := sandbox(t, appID)
	asked := 0
	s.admin = func(command, why string) error { asked++; return asUser(command, why) }
	s.security = func(...string) (string, error) { return "", errors.New("not trusted") }
	if state := s.domainState(); state.On || state.Suffix != "fabo.localhost" || state.HTTPS {
		t.Fatalf("before: %+v", state)
	}
	for range 2 {
		if err := s.localNames(true); err != nil {
			t.Fatal(err)
		}
	}
	raw, _ := os.ReadFile(s.root + resolverPath)
	if asked != 1 || !strings.Contains(string(raw), "nameserver 127.0.0.1\nport 53535\n") {
		t.Fatalf("asked %d times, wrote %q", asked, raw)
	}
	if state := s.domainState(); !state.On || state.Suffix != "fabo.local" {
		t.Fatalf("after: %+v", state)
	}
	for range 2 {
		if err := s.localNames(false); err != nil {
			t.Fatal(err)
		}
	}
	if asked != 2 || exists(s.root+resolverPath) {
		t.Fatalf("asked %d times, rule still there: %v", asked, exists(s.root+resolverPath))
	}
}

// *.fabo.local is the installed app's: a flavored build neither writes nor
// removes the rule, and a closed prompt changes nothing.
func TestLocalNamesStayTheInstalledApps(t *testing.T) {
	s := sandbox(t, "com.openfabrica.fabo.dev")
	s.admin = func(string, string) error { t.Fatal("a flavored app asked for the password"); return nil }
	write(t, s.root+resolverPath, "nameserver 127.0.0.1\nport 53535\n")
	for _, on := range []bool{true, false} {
		if err := s.localNames(on); err == nil || !strings.Contains(err.Error(), "Fabo Dev") {
			t.Fatalf("on=%v: %v", on, err)
		}
	}
	if !exists(s.root + resolverPath) {
		t.Fatal("the installed app's rule was removed")
	}
	s = sandbox(t, appID)
	s.admin = func(string, string) error { return errCanceled }
	if err := s.localNames(true); !errors.Is(err, errCanceled) || s.namesInstalled() {
		t.Fatalf("canceled prompt: %v", err)
	}
}

// Trusting needs the engine's authority to exist, trusts that file for
// websites, and removal names the certificate by fingerprint so another
// installation's authority of the same name is left alone.
func TestHTTPSTrustsThisInstallationsAuthority(t *testing.T) {
	s := sandbox(t, appID)
	var calls []string
	trusted := false
	s.security = func(args ...string) (string, error) {
		calls = append(calls, strings.Join(args, " "))
		switch args[0] {
		case "verify-cert":
			if !trusted {
				return "", errors.New("exit status 1")
			}
		case "add-trusted-cert":
			trusted = true
		case "remove-trusted-cert":
			trusted = false
		}
		return "", nil
	}
	if err := s.https(true); err == nil {
		t.Fatal("trusted an authority that does not exist")
	}
	ca := filepath.Join(s.in.home, ".fabo/tls/ca.pem")
	// PEM around the three bytes 01 02 03.
	write(t, ca, "-----BEGIN CERTIFICATE-----\nAQID\n-----END CERTIFICATE-----\n")
	if err := s.https(true); err != nil || !s.domainState().HTTPS {
		t.Fatalf("trust: %v %v", err, calls)
	}
	if want := "add-trusted-cert -r trustRoot -p ssl -k " + s.keychain() + " " + ca; !contains(calls, want) {
		t.Fatalf("no %q in %v", want, calls)
	}
	calls = nil
	if err := s.https(false); err != nil || s.domainState().HTTPS {
		t.Fatalf("untrust: %v %v", err, calls)
	}
	if want := "delete-certificate -Z 7037807198C22A7D2B0807371D763779A84FDFCF " + s.keychain(); !contains(calls, "remove-trusted-cert "+ca) || !contains(calls, want) {
		t.Fatalf("untrust ran %v", calls)
	}
	s.security = func(...string) (string, error) {
		return "SecTrustSettingsSetTrustSettings: The authorization was canceled by the user.", errors.New("exit status 1")
	}
	if err := s.https(true); !errors.Is(err, errCanceled) {
		t.Fatalf("canceled prompt: %v", err)
	}
}

// contains reports whether list holds s.
func contains(list []string, s string) bool {
	for _, item := range list {
		if item == s {
			return true
		}
	}
	return false
}
