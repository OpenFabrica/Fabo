package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// `fabo setup` makes this Mac use the engine, without touching anything
// another engine owns:
//
//   - a docker context for this installation (--use also makes it current)
//   - a LaunchAgent that starts `fabo daemon` at login (--no-agent skips it)
//   - `docker` and `fabo` in ~/.kiri/bin, which a marked line in the shell
//     profiles adds to the end of PATH, so a docker CLI already installed
//     keeps priority; and /usr/local/bin/fabo when that directory is the user's
//   - on a Mac with no other Docker engine, this context as docker's default
//   - a buildx builder that loads what it builds into the engine, and the
//     choice of builder for plain `docker build` (buildwith_darwin.go)
//   - ~/.testcontainers.properties while /var/run/docker.sock is free,
//     unless the user has a file of their own there
//   - with --domains, /etc/resolver/kiri.local (domains_darwin.go), which
//     asks for an administrator's password
//
// `fabo setup --uninstall` takes back exactly those.
//
// A flavored app sets up only its own: its context, LaunchAgent, builder
// and bin directory. PATH, docker's default context and Testcontainers stay
// the installed app's.

// setup is one installation's view of the files setup changes.
type setup struct {
	in     installation
	docker string // docker's config directory
	// root is prepended to the machine-wide paths (/var/run/docker.sock,
	// /Applications, /usr/local/bin). Empty outside tests.
	root string
	out  io.Writer
	// admin and security replace the password prompt and /usr/bin/security
	// in tests. Nil outside them.
	admin    func(command, why string) error
	security func(args ...string) (string, error)
}

// newSetup locates docker's config directory for the installation.
func newSetup(in installation) *setup {
	docker := os.Getenv("DOCKER_CONFIG")
	if docker == "" {
		docker = filepath.Join(in.home, ".docker")
	}
	return &setup{in: in, docker: docker, out: os.Stdout}
}

// contents is the app bundle's Contents directory, or "" outside a bundle.
func (s *setup) contents() string {
	contents := filepath.Dir(s.in.dir)
	if !isFile(filepath.Join(contents, "Info.plist")) {
		return ""
	}
	return contents
}

// setupCommand is `fabo setup [--use] [--no-agent] | --domains | --uninstall`.
func setupCommand(args []string) error {
	in, err := locate()
	if err != nil {
		return err
	}
	return newSetup(in).run(args)
}

// buildWithCommand is `fabo build-with [docker|fabo]`.
func buildWithCommand(args []string) error {
	in, err := locate()
	if err != nil {
		return err
	}
	return newSetup(in).buildWith(args)
}

// run performs setup, or with --uninstall takes it back.
func (s *setup) run(args []string) error {
	flags := map[string]bool{}
	for _, arg := range args {
		switch arg {
		case "--use", "--no-agent", "--no-domains", "--uninstall":
			flags[arg] = true
		case "--domains":
			if err := s.localNames(true); err != nil {
				return err
			}
			fmt.Fprintln(s.out, "containers answer at http://<name>.kiri.local")
			return nil
		case "--docker-socket":
			return fmt.Errorf("fabo setup %s is not available in this build yet", arg)
		default:
			return errors.New("usage: fabo setup [--use] [--no-agent] | fabo setup --domains | fabo setup --uninstall")
		}
	}
	if flags["--uninstall"] {
		return s.uninstall()
	}
	// An installation that cannot start is not set up either. This is what
	// keeps a build from taking the LaunchAgent and the docker context away
	// from a Kiri (Rust) installation whose data has not been imported.
	if _, _, err := s.in.daemonCommand(); err != nil {
		return err
	}
	n := s.in.names
	if err := s.writeContext(); err != nil {
		return err
	}
	fmt.Fprintf(s.out, "docker context `%s` → unix://%s\n", n.context, s.in.socket())
	if flags["--use"] && !n.flavored {
		if err := s.useContext(); err != nil {
			return err
		}
		fmt.Fprintln(s.out, "docker now uses Fabo by default (`docker context use default` to switch back)")
	}
	if contents := s.contents(); contents != "" {
		if err := s.bundledCLI(contents); err != nil {
			return err
		}
	}
	if !flags["--no-agent"] {
		plist, err := s.installAgent()
		if err != nil {
			return err
		}
		fmt.Fprintf(s.out, "fabo daemon starts at login (%s)\n", plist)
	}
	switch made, err := s.ensureBuilder(); {
	case err != nil:
		fmt.Fprintf(s.out, "docker build: the `%s` builder comes later (%v); opening the app makes it once Fabo is running\n", n.builder, err)
	case made && s.places().saved() == "kiri":
		fmt.Fprintf(s.out, "docker build uses BuildKit in Fabo (builder `%s`)\n", n.builder)
	case made:
		fmt.Fprintln(s.out, "docker build follows docker's context, and builds on Fabo when docker uses Fabo (`fabo build-with fabo` sends every build to Fabo)")
	}
	if !n.flavored {
		fmt.Fprintln(s.out, s.socketNote())
	}
	return nil
}

// launchedFromApp reports whether this process is the app being opened from
// Finder or the Dock: the bundle's binary, started by launchd, not from a
// shell, where running it bare must not change anything.
func (s *setup) launchedFromApp() bool {
	return s.contents() != "" && os.Getppid() == 1
}

// firstLaunch is what opening the app does before the window: the same as
// `fabo setup`, quietly. Once the LaunchAgent starts this copy of the app,
// only the command links are refreshed.
func (s *setup) firstLaunch() error {
	s.out = io.Discard
	if s.agentRunsThis() {
		if contents := s.contents(); contents != "" {
			_ = s.bundledCLI(contents)
		}
		if !s.in.names.flavored {
			_ = s.socketNote()
		}
		return nil
	}
	return s.run(nil)
}

// uninstall takes back what setup did. The engine's data stays.
func (s *setup) uninstall() error {
	if err := s.removeUSBHelper(); err != nil {
		return err
	}
	n := s.in.names
	// Uninstalling stops everything this app runs: the engine the agent
	// started, and any engine or window started another way.
	s.bootout()
	s.stopOthers()
	if err := os.Remove(s.agentPath()); err == nil {
		fmt.Fprintln(s.out, "removed the LaunchAgent")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := s.forgetDocker(); err != nil {
		return err
	}
	// Docker's builder back as it was, before Kiri's goes.
	if err := s.places().uninstall(); err != nil {
		fmt.Fprintf(s.out, "could not give docker build back its own builder (%v)\n", err)
	}
	s.removeBuilder()
	if !n.flavored {
		s.unlinkSystem()
	}
	_ = os.Remove(filepath.Join(s.binDir(), "docker"))
	_ = os.Remove(filepath.Join(s.binDir(), "fabo"))
	if !n.flavored {
		if err := s.removeShellPath(); err != nil {
			fmt.Fprintf(s.out, "could not tidy the shell profiles (%v)\n", err)
		} else {
			fmt.Fprintln(s.out, "removed ~/.kiri/bin from shell profiles")
		}
		_, _ = testcontainers(filepath.Join(s.in.home, ".testcontainers.properties"), s.in.socket(), false)
		if err := s.localNames(false); err != nil {
			fmt.Fprintf(s.out, "could not remove %s (%v)\n", resolverPath, err)
		}
	}
	fmt.Fprintf(s.out, "%s's data stays in %s; delete it to remove images and volumes too\n", n.app, filepath.Join(s.in.root(), "go-engine"))
	return nil
}

// stopOthers asks every other process running this app's executables to
// quit, and makes them after the engine has had time to stop its containers.
func (s *setup) stopOthers() {
	pattern := "^" + regexp.QuoteMeta(s.in.dir) + "/(fabo|kiri|kiri-go-daemon|kiri-desktop)( |$)"
	me := fmt.Sprint(os.Getpid())
	others := func() []string {
		out, _ := exec.Command("/usr/bin/pgrep", "-f", pattern).Output()
		var pids []string
		for _, pid := range strings.Fields(string(out)) {
			if pid != me {
				pids = append(pids, pid)
			}
		}
		return pids
	}
	pids := others()
	if len(pids) == 0 {
		return
	}
	_ = exec.Command("/bin/kill", append([]string{"-TERM"}, pids...)...).Run()
	// The engine gives its containers up to 25 seconds to stop.
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(250 * time.Millisecond) {
		if pids = others(); len(pids) == 0 {
			return
		}
	}
	_ = exec.Command("/bin/kill", append([]string{"-KILL"}, pids...)...).Run()
}

// reportFailure shows why the app did not open. Opened from Finder there is
// no terminal to read the error in.
func reportFailure(err error) {
	in, lerr := locate()
	if lerr != nil || !newSetup(in).launchedFromApp() {
		return
	}
	quote := func(text string) string {
		return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(text) + `"`
	}
	script := "display alert " + quote(in.names.app+" could not start") + " message " + quote(err.Error()) + " as critical"
	_ = exec.Command("/usr/bin/osascript", "-e", script).Run()
}
