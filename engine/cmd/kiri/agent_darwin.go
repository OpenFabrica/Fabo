package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const launchctl = "/bin/launchctl"

// agentPath is the LaunchAgent that starts this installation's engine at login.
func (s *setup) agentPath() string {
	return filepath.Join(s.in.home, "Library", "LaunchAgents", s.in.names.agent+".plist")
}

// launcher is this installation's `kiri`, the program the agent runs.
func (s *setup) launcher() string { return filepath.Join(s.in.dir, "kiri") }

// xmlText escapes a path for a plist string.
func xmlText(text string) string {
	return strings.ReplaceAll(strings.ReplaceAll(text, "&", "&amp;"), "<", "&lt;")
}

// agentProgram is how the plist names the launcher. agentRunsThis looks for
// exactly this, so both must agree.
func (s *setup) agentProgram() string {
	return "<string>" + xmlText(s.launcher()) + "</string><string>daemon</string>"
}

// agentPlist is a LaunchAgent for `kiri daemon`: started at login, restarted
// if it crashes, never restarted after a clean exit. The bundle identifier
// makes macOS say the app's name in Login Items rather than the name on the
// signing certificate.
func (s *setup) agentPlist() string {
	log := xmlText(filepath.Join(s.in.root(), "daemon.log"))
	return `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>` + s.in.names.agent + `</string>
  <key>AssociatedBundleIdentifiers</key><array><string>` + s.in.names.id + `</string></array>
  <key>ProgramArguments</key><array>` + s.agentProgram() + `</array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><dict><key>SuccessfulExit</key><false/></dict>
  <key>ProcessType</key><string>Interactive</string>
  <key>StandardOutPath</key><string>` + log + `</string>
  <key>StandardErrorPath</key><string>` + log + `</string>
</dict>
</plist>
`
}

// agentRunsThis reports whether the LaunchAgent starts this copy of the
// app. One left by another copy (an older download, a build elsewhere) is
// replaced when this one is set up.
func (s *setup) agentRunsThis() bool {
	raw, err := os.ReadFile(s.agentPath())
	return err == nil && strings.Contains(string(raw), s.agentProgram())
}

// domain is launchd's name for this user's login session.
func domain() string { return "gui/" + strconv.Itoa(os.Getuid()) }

// installAgent writes the LaunchAgent and loads it, which starts the engine.
func (s *setup) installAgent() (string, error) {
	path := s.agentPath()
	for _, dir := range []string{filepath.Dir(path), s.in.root()} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", err
		}
	}
	if err := os.WriteFile(path, []byte(s.agentPlist()), 0o644); err != nil {
		return "", err
	}
	// Reload, so a new path to the binary takes effect.
	s.bootout()
	// bootout returns before launchd has let the old job go, and a bootstrap
	// in that moment fails (error 5): try again for a few seconds.
	for attempt := 0; attempt < 20; attempt++ {
		if exec.Command(launchctl, "bootstrap", domain(), path).Run() == nil {
			return path, nil
		}
		time.Sleep(250 * time.Millisecond)
	}
	return "", fmt.Errorf("launchctl could not load %s", path)
}

// bootout unloads the agent, which stops the engine it started.
func (s *setup) bootout() {
	_ = exec.Command(launchctl, "bootout", domain()+"/"+s.in.names.agent).Run()
}

// kickstart asks launchd to start the loaded agent if it is not running.
func (s *setup) kickstart() error {
	return exec.Command(launchctl, "kickstart", domain()+"/"+s.in.names.agent).Run()
}

// agentLoaded reports whether launchd has the agent, running or not.
func (s *setup) agentLoaded() bool {
	return exec.Command(launchctl, "print", domain()+"/"+s.in.names.agent).Run() == nil
}
