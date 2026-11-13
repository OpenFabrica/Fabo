package main

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Container names and HTTPS, the two machine-level halves of Settings →
// Domains (upstream setup.rs `domains` / `remove_domains`, tls.rs `trust` /
// `untrust`):
//
//   - `*.kiri.local` works once /etc/resolver/kiri.local sends those lookups
//     to the engine's DNS answerer on 127.0.0.1:53535. Writing it needs an
//     administrator. Without it containers answer at `*.kiri.localhost`,
//     which needs no setup.
//   - `https://` works once this user trusts the engine's certificate
//     authority (<state>/tls/ca.pem) for websites. Its name constraints let
//     it vouch for the two suffixes above and nothing else.
const (
	resolverPath = "/etc/resolver/kiri.local"
	// resolverPort is where the engine answers *.kiri.local (domains.go).
	resolverPort = "53535"
)

// errCanceled is a password prompt the user closed: a choice, not a failure.
var errCanceled = errors.New("canceled")

// domainState is what `fabo domains` prints for the desktop window.
type domainState struct {
	On     bool   `json:"on"`     // *.kiri.local resolves to the engine
	Suffix string `json:"suffix"` // the suffix links should carry
	HTTPS  bool   `json:"https"`  // the authority is trusted for websites
}

// domainsCommand is `fabo domains [local on|off | https on|off]`. With no
// argument it prints the state as JSON.
func domainsCommand(args []string) error {
	in, err := locate()
	if err != nil {
		return err
	}
	s := newSetup(in)
	on := len(args) == 2 && args[1] == "on"
	switch {
	case len(args) == 0:
		return json.NewEncoder(s.out).Encode(s.domainState())
	case len(args) != 2 || !on && args[1] != "off":
	case args[0] == "local":
		return s.localNames(on)
	case args[0] == "https":
		return s.https(on)
	}
	return errors.New("usage: fabo domains [local on|off | https on|off]")
}

// domainState reads both settings from the machine.
func (s *setup) domainState() domainState {
	state := domainState{On: s.namesInstalled(), Suffix: "kiri.localhost", HTTPS: s.httpsTrusted()}
	if state.On {
		state.Suffix = "kiri.local"
	}
	return state
}

// namesInstalled reports whether macOS sends *.kiri.local lookups to Kiri.
func (s *setup) namesInstalled() bool {
	raw, err := os.ReadFile(s.root + resolverPath)
	return err == nil && strings.Contains(string(raw), "port "+resolverPort)
}

// localNames installs (on) or removes the resolver rule for *.kiri.local.
// The rule is one per machine and belongs to the installed app: a flavored
// build changing it would take the names away from that app's containers.
func (s *setup) localNames(on bool) error {
	if s.in.names.flavored {
		return fmt.Errorf("*.kiri.local belongs to the installed Kiri, not %s", s.in.names.app)
	}
	path := s.root + resolverPath
	if on {
		if s.namesInstalled() {
			return nil
		}
		tmp, err := os.CreateTemp("", "kiri-resolver-")
		if err != nil {
			return err
		}
		defer os.Remove(tmp.Name())
		_, err = tmp.WriteString("# Kiri: container names (fabo setup --uninstall removes this)\nnameserver 127.0.0.1\nport " + resolverPort + "\n")
		if closeErr := tmp.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			return err
		}
		return s.asAdmin(fmt.Sprintf("mkdir -p %s && cp %s %s && chmod 644 %s", shellQuote(filepath.Dir(path)), shellQuote(tmp.Name()), shellQuote(path), shellQuote(path)),
			"Kiri wants to give your containers names like myapp.kiri.local.")
	}
	if !exists(path) {
		return nil
	}
	return s.asAdmin("rm -f "+shellQuote(path), "Kiri wants to remove its *.kiri.local names.")
}

// shellQuote quotes s as one word of a POSIX shell command.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// asAdmin runs a shell command as root through macOS's own password prompt,
// which shows why. Closing the prompt gives errCanceled.
func (s *setup) asAdmin(command, why string) error {
	if s.admin != nil {
		return s.admin(command, why)
	}
	escape := strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace
	script := fmt.Sprintf(`do shell script "%s" with prompt "%s" with administrator privileges`, escape(command), escape(why))
	out, err := exec.Command("/usr/bin/osascript", "-e", script).CombinedOutput()
	if err == nil {
		return nil
	}
	// AppleScript reports a closed prompt as error -128.
	if strings.Contains(string(out), "-128") {
		return errCanceled
	}
	return errors.New(strings.TrimSpace(string(out)))
}

// caPath is the engine's certificate authority, made when it first serves
// HTTPS names.
func (s *setup) caPath() string { return filepath.Join(s.in.root(), "tls", "ca.pem") }

// keychain is where the authority is kept so Keychain Access shows it.
func (s *setup) keychain() string {
	return filepath.Join(s.in.home, "Library/Keychains/login.keychain-db")
}

// runSecurity runs /usr/bin/security and returns what it printed.
func (s *setup) runSecurity(args ...string) (string, error) {
	if s.security != nil {
		return s.security(args...)
	}
	out, err := exec.Command("/usr/bin/security", args...).CombinedOutput()
	return string(out), err
}

// httpsTrusted reports whether this user trusts this installation's
// authority for websites. The check is of the certificate itself, not its
// name: every installation's authority has the same name.
func (s *setup) httpsTrusted() bool {
	if !isFile(s.caPath()) {
		return false
	}
	_, err := s.runSecurity("verify-cert", "-c", s.caPath(), "-p", "ssl", "-L")
	return err == nil
}

// https trusts (on) the authority as a root for websites only, for this
// user, or takes that back and removes it from the keychain. macOS asks for
// the password itself.
func (s *setup) https(on bool) error {
	ca := s.caPath()
	raw, err := os.ReadFile(ca)
	if err != nil {
		if !on {
			return nil
		}
		return errors.New("Kiri has not made its certificate yet: start the engine with its HTTPS port in use, then try again")
	}
	if on {
		// Fails harmlessly when the certificate is already in the keychain.
		_, _ = s.runSecurity("add-certificates", "-k", s.keychain(), ca)
		return securityError(s.runSecurity("add-trusted-cert", "-r", "trustRoot", "-p", "ssl", "-k", s.keychain(), ca))
	}
	if s.httpsTrusted() {
		if err := securityError(s.runSecurity("remove-trusted-cert", ca)); err != nil {
			return err
		}
	}
	// By fingerprint: deleting by name would also remove the authority of
	// another installation, which is called the same.
	if block, _ := pem.Decode(raw); block != nil {
		sum := sha1.Sum(block.Bytes)
		_, _ = s.runSecurity("delete-certificate", "-Z", strings.ToUpper(hex.EncodeToString(sum[:])), s.keychain())
	}
	return nil
}

// securityError turns a failed /usr/bin/security run into its message, and
// a closed password prompt into errCanceled.
func securityError(out string, err error) error {
	if err == nil {
		return nil
	}
	if strings.Contains(strings.ToLower(out), "cancel") {
		return errCanceled
	}
	if text := strings.TrimSpace(out); text != "" {
		return errors.New(text)
	}
	return err
}
