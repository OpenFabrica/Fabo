package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// profileMark retains the installed ownership marker so existing profiles
// are recognized and uninstall can remove the same marked block.
const profileMark = "# Added by Fabo: its docker and fabo commands (fabo setup --uninstall removes this)"

// shLine adds ~/.fabo/bin to the end of PATH once, however many of the
// profiles a shell reads. Plain sh, for zsh and bash alike. At the end, so a
// docker CLI already installed keeps priority.
const shLine = `case ":$PATH:" in *":$HOME/.fabo/bin:"*) ;; *) export PATH="$PATH:$HOME/.fabo/bin" ;; esac`

// oldShLine is the line Fabo 0.1.0 wrote; taken back on uninstall.
const oldShLine = `export PATH="$PATH:$HOME/.fabo/bin"`

const fishLine = "fish_add_path --append $HOME/.fabo/bin"

// systemLink is /usr/local/bin/fabo: on every shell's PATH already, so
// `fabo` works even in a terminal opened before the app was.
const systemLink = "/usr/local/bin/fabo"

// binDir is ~/.fabo/bin, or a flavor's own.
func (s *setup) binDir() string { return filepath.Join(s.in.root(), "bin") }

// bundledCLI links `fabo` and the app's `docker` into the bin directory,
// puts that directory on PATH, and adds docker's plugins (Compose, buildx)
// to the CLI's plugin search path. A flavored app's commands are run by
// path and its plugins stay its own.
func (s *setup) bundledCLI(contents string) error {
	bin := s.binDir()
	if err := os.MkdirAll(bin, 0o755); err != nil {
		return err
	}
	fabo := filepath.Join(bin, "fabo")
	if err := relink(filepath.Join(contents, "MacOS", "fabo"), fabo); err != nil {
		return err
	}
	if !s.in.names.flavored {
		if err := s.shellPath(); err != nil {
			return err
		}
		s.linkSystem(fabo)
	}
	tools := filepath.Join(contents, "Resources", "bin")
	if !isFile(filepath.Join(tools, "docker")) {
		return nil
	}
	if err := relink(filepath.Join(tools, "docker"), filepath.Join(bin, "docker")); err != nil {
		return err
	}
	if s.in.names.flavored {
		fmt.Fprintf(s.out, "docker and fabo commands: %s\n", bin)
		return nil
	}
	if err := s.defaultContext(); err != nil {
		return err
	}
	if err := s.addPluginDir(filepath.Join(tools, "cli-plugins")); err != nil {
		return err
	}
	fmt.Fprintf(s.out, "docker and fabo commands: %s (on PATH in new terminal windows)\n", bin)
	return nil
}

// relink points link at target, replacing whatever link was there.
func relink(target, link string) error {
	_ = os.Remove(link)
	return os.Symlink(target, link)
}

// linkSystem makes /usr/local/bin/fabo when the user can write there
// (Homebrew on Intel Macs makes it theirs). Missing, or a link an older Fabo
// left, it is made again to this one. Anything else there is not Fabo's to
// replace.
func (s *setup) linkSystem(fabo string) {
	link := s.root + systemLink
	replace := false
	if target, err := os.Readlink(link); err == nil {
		replace = target != fabo && (faboLink(target) || !exists(link))
	} else if _, err := os.Lstat(link); err != nil {
		replace = true
	}
	if replace {
		_ = relink(fabo, link)
	}
}

// faboLink reports whether a link target is a Fabo installation's command: inside an
// app bundle, or a Fabo bin directory.
func faboLink(target string) bool {
	return strings.HasSuffix(target, "/fabo") &&
		(strings.Contains(target, ".app/Contents/MacOS/") || strings.Contains(target, "/.fabo/bin/") || strings.Contains(target, "/.fabo-dev/bin/"))
}

// unlinkSystem removes /usr/local/bin/fabo when it is the link linkSystem makes.
func (s *setup) unlinkSystem() {
	link := s.root + systemLink
	if target, err := os.Readlink(link); err == nil && target == filepath.Join(s.binDir(), "fabo") {
		_ = os.Remove(link)
	}
}

// profile is a shell profile and the line that belongs in it.
type profile struct{ file, line string }

// profiles are the shell profiles Fabo puts ~/.fabo/bin in, so `fabo` is
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
		list = append(list, profile{filepath.Join(home, ".config/fish/conf.d/fabo.fish"), fishLine})
	}
	return list
}

// shellPath appends the marked PATH line to each profile that lacks it. A
// profile that can't be read or written is passed over (and logged), not the
// end of setup: a link to a dotfiles folder that moved, or a read-only link
// Nix home-manager made, failed every first launch of the app.
func (s *setup) shellPath() error {
	for _, p := range s.profiles() {
		// A link to nothing: writing through it fails when the target's
		// folder is gone, or makes a file where it points. It stays as the
		// user left it.
		if _, err := os.Lstat(p.file); err == nil && !exists(p.file) {
			skipProfile(p.file, errors.New("a link to nothing"))
			continue
		}
		// Only a profile Fabo could read, or one that isn't there, is
		// written: a read error must never turn into a profile holding only
		// Fabo's lines.
		raw, err := os.ReadFile(p.file)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			skipProfile(p.file, err)
			continue
		}
		text := string(raw)
		if strings.Contains(text, profileMark) {
			continue
		}
		sep := "\n"
		if text == "" || strings.HasSuffix(text, "\n") {
			sep = ""
		}
		err = os.MkdirAll(filepath.Dir(p.file), 0o755)
		if err == nil {
			err = writeProfile(p.file, text+sep+"\n"+profileMark+"\n"+p.line+"\n")
		}
		if err != nil {
			skipProfile(p.file, err)
		}
	}
	return nil
}

// skipProfile logs a shell profile setup leaves as it is. Standard error is
// the log: setup's own output is discarded when the app is opened.
func skipProfile(file string, err error) {
	fmt.Fprintf(os.Stderr, "fabo: shell profile %s skipped: %v\n", file, err)
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
		// profile edited since. oldShLine is what Fabo 0.1.0 wrote.
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
