package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// profileMark is the marked line Kiri adds to shell profiles, and removes on uninstall.
const profileMark = "# Added by Kiri: its docker and kiri commands (kiri setup --uninstall removes this)"

// shLine adds ~/.kiri/bin to the end of PATH once, however many of the
// profiles a shell reads. Plain sh, for zsh and bash alike. At the end, so a
// docker CLI already installed keeps priority.
const shLine = `case ":$PATH:" in *":$HOME/.kiri/bin:"*) ;; *) export PATH="$PATH:$HOME/.kiri/bin" ;; esac`

// oldShLine is the line Kiri 0.1.0 wrote; taken back on uninstall.
const oldShLine = `export PATH="$PATH:$HOME/.kiri/bin"`

const fishLine = "fish_add_path --append $HOME/.kiri/bin"

// systemLink is /usr/local/bin/kiri: on every shell's PATH already, so
// `kiri` works even in a terminal opened before the app was.
const systemLink = "/usr/local/bin/kiri"

// binDir is ~/.kiri/bin, or a flavor's own.
func (s *setup) binDir() string { return filepath.Join(s.in.root(), "bin") }

// bundledCLI links `kiri` and the app's `docker` into the bin directory,
// puts that directory on PATH, and adds docker's plugins (Compose, buildx)
// to the CLI's plugin search path. A flavored app's commands are run by
// path and its plugins stay its own.
func (s *setup) bundledCLI(contents string) error {
	bin := s.binDir()
	if err := os.MkdirAll(bin, 0o755); err != nil {
		return err
	}
	kiri := filepath.Join(bin, "kiri")
	if err := relink(filepath.Join(contents, "MacOS", "kiri"), kiri); err != nil {
		return err
	}
	if !s.in.names.flavored {
		if err := s.shellPath(); err != nil {
			return err
		}
		s.linkSystem(kiri)
	}
	tools := filepath.Join(contents, "Resources", "bin")
	if !isFile(filepath.Join(tools, "docker")) {
		return nil
	}
	if err := relink(filepath.Join(tools, "docker"), filepath.Join(bin, "docker")); err != nil {
		return err
	}
	if s.in.names.flavored {
		fmt.Fprintf(s.out, "docker and kiri commands: %s\n", bin)
		return nil
	}
	if err := s.defaultContext(); err != nil {
		return err
	}
	if err := s.addPluginDir(filepath.Join(tools, "cli-plugins")); err != nil {
		return err
	}
	fmt.Fprintf(s.out, "docker and kiri commands: %s (on PATH in new terminal windows)\n", bin)
	return nil
}

// relink points link at target, replacing whatever link was there.
func relink(target, link string) error {
	_ = os.Remove(link)
	return os.Symlink(target, link)
}

// linkSystem makes /usr/local/bin/kiri when the user can write there
// (Homebrew on Intel Macs makes it theirs). Missing, or a link an older Kiri
// left, it is made again to this one. Anything else there is not Kiri's to
// replace.
func (s *setup) linkSystem(kiri string) {
	link := s.root + systemLink
	replace := false
	if target, err := os.Readlink(link); err == nil {
		replace = target != kiri && (kiriLink(target) || !exists(link))
	} else if _, err := os.Lstat(link); err != nil {
		replace = true
	}
	if replace {
		_ = relink(kiri, link)
	}
}

// kiriLink reports whether a link target is some Kiri's `kiri`: inside an
// app bundle, or a Kiri bin directory.
func kiriLink(target string) bool {
	return strings.HasSuffix(target, "/kiri") &&
		(strings.Contains(target, ".app/Contents/MacOS/") || strings.Contains(target, "/.kiri/bin/") || strings.Contains(target, "/.kiri-dev/bin/"))
}

// unlinkSystem removes /usr/local/bin/kiri when it is the link linkSystem makes.
func (s *setup) unlinkSystem() {
	link := s.root + systemLink
	if target, err := os.Readlink(link); err == nil && target == filepath.Join(s.binDir(), "kiri") {
		_ = os.Remove(link)
	}
}

// profile is a shell profile and the line that belongs in it.
type profile struct{ file, line string }

// profiles are the shell profiles Kiri puts ~/.kiri/bin in, so `kiri` is
// found in every terminal:
//   - zsh (the macOS default) reads .zprofile only in login shells, which
//     Terminal opens but VS Code, tmux and IDE terminals do not, and a
//     .zshrc that sets PATH runs after it: both files, in $ZDOTDIR if set
//   - bash reads the first of .bash_profile, .bash_login and .profile in
//     login shells (.bash_profile is made if there is none) and .bashrc in
//     others, when the user has one
//   - fish's conf.d, when the user has fish
func (s *setup) profiles() []profile {
	home, zdotdir := s.in.home, os.Getenv("ZDOTDIR")
	if zdotdir == "" {
		zdotdir = home
	}
	list := []profile{{filepath.Join(zdotdir, ".zprofile"), shLine}, {filepath.Join(zdotdir, ".zshrc"), shLine}}
	login := filepath.Join(home, ".bash_profile")
	for _, name := range []string{".bash_profile", ".bash_login", ".profile"} {
		if isFile(filepath.Join(home, name)) {
			login = filepath.Join(home, name)
			break
		}
	}
	list = append(list, profile{login, shLine})
	if isFile(filepath.Join(home, ".bashrc")) {
		list = append(list, profile{filepath.Join(home, ".bashrc"), shLine})
	}
	if info, err := os.Stat(filepath.Join(home, ".config/fish")); err == nil && info.IsDir() {
		list = append(list, profile{filepath.Join(home, ".config/fish/conf.d/kiri.fish"), fishLine})
	}
	return list
}

// shellPath appends the marked PATH line to each profile that lacks it.
func (s *setup) shellPath() error {
	for _, p := range s.profiles() {
		raw, _ := os.ReadFile(p.file)
		text := string(raw)
		if strings.Contains(text, profileMark) {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(p.file), 0o755); err != nil {
			return err
		}
		sep := "\n"
		if text == "" || strings.HasSuffix(text, "\n") {
			sep = ""
		}
		if err := writeProfile(p.file, text+sep+"\n"+profileMark+"\n"+p.line+"\n"); err != nil {
			return err
		}
	}
	return nil
}

// removeShellPath takes the marked lines back out, and removes the fish
// file when nothing else is left in it.
func (s *setup) removeShellPath() error {
	for _, p := range s.profiles() {
		raw, err := os.ReadFile(p.file)
		if err != nil || !strings.Contains(string(raw), profileMark) {
			continue
		}
		// With the blank line setup put before it, then without, for a
		// profile edited since. oldShLine is what Kiri 0.1.0 wrote.
		kept := string(raw)
		for _, line := range []string{p.line, oldShLine} {
			block := profileMark + "\n" + line + "\n"
			kept = strings.ReplaceAll(strings.ReplaceAll(kept, "\n"+block, ""), block, "")
		}
		if strings.TrimSpace(kept) == "" && filepath.Ext(p.file) == ".fish" {
			err = os.Remove(p.file)
		} else {
			err = writeProfile(p.file, kept)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// writeProfile rewrites a profile in place: through a symlink when the file
// is one, and with the permissions it already has.
func writeProfile(file, text string) error {
	return os.WriteFile(file, []byte(text), 0o644)
}
