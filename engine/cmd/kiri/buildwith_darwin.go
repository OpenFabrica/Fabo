package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
)

// Which builder plain `docker build` uses: Docker's own, or Kiri's.
//
// The docker CLI hands `docker build` to buildx with BUILDX_BUILDER set to
// the current context, unless the config's `builder` alias is set (what
// `docker buildx install` does), in which case buildx takes its current
// builder (~/.docker/buildx/current). Compose and `docker buildx build`
// always take buildx's current builder.
//
//   - Always: a buildx builder named after Kiri's context, a copy of Kiri's
//     builder. A builder in buildx's store wins over the context of the same
//     name, so `docker --context kiri build` builds on Kiri and loads into
//     Kiri. No other context is touched.
//   - Build with Docker: nothing more. Builds follow docker's context.
//   - Build with Kiri: the alias, and Kiri's builder as buildx's global
//     current builder. Every build runs on Kiri whatever context is in use.
//
// What "Build with Kiri" changes is kept in build.json first, and choosing
// Docker puts it back exactly, unless something else has changed it since.

// buildPlaces is where everything is: docker's config directory, buildx's,
// Kiri's build.json, and Kiri's builder and context names.
type buildPlaces struct {
	docker, buildx, state string
	builder, context      string
}

func (p buildPlaces) config() string  { return filepath.Join(p.docker, "config.json") }
func (p buildPlaces) current() string { return filepath.Join(p.buildx, "current") }

// instance is buildx's record of the builder called name.
func (p buildPlaces) instance(name string) string {
	return filepath.Join(p.buildx, "instances", name)
}

// readState is build.json; unreadable counts as empty.
func (p buildPlaces) readState() object {
	s, err := readJSON(p.state)
	if err != nil {
		return object{}
	}
	return s
}

// saved is what was chosen, "docker" or "kiri", or "" when nobody has.
func (p buildPlaces) saved() string {
	if with, _ := p.readState()["with"].(string); with == "docker" || with == "kiri" {
		return with
	}
	return ""
}

// ready reports whether Kiri's builder is in buildx's store yet.
func (p buildPlaces) ready() bool { return isFile(p.instance(p.builder)) }

// shadow keeps the builder named after Kiri's context a copy of Kiri's
// builder. One of that name that is not such a copy is left alone.
func (p buildPlaces) shadow() error {
	ours, err := readJSON(p.instance(p.builder))
	if err != nil {
		return err
	}
	if _, made := ours["Name"]; !made {
		return nil
	}
	path := p.instance(p.context)
	theirs, err := readJSON(path)
	if err != nil {
		return err
	}
	if _, named := theirs["Name"]; named && !p.copied(theirs) {
		return nil
	}
	ours["Name"] = p.context
	if reflect.DeepEqual(theirs, ours) {
		return nil
	}
	raw, err := encodeJSON(ours, false)
	if err != nil {
		return err
	}
	return writeFile(path, raw, 0o600)
}

// copied reports whether a builder is Kiri's copy: its nodes are Kiri's builder's.
func (p buildPlaces) copied(instance object) bool {
	nodes, _ := instance["Nodes"].([]any)
	for _, node := range nodes {
		n, _ := node.(object)
		if name, _ := n["Name"].(string); !strings.HasPrefix(name, p.builder) {
			return false
		}
	}
	return len(nodes) > 0
}

// save records the choice. auto marks one made without asking.
func (p buildPlaces) save(with string, auto bool) error {
	s := p.readState()
	s["with"], s["auto"] = with, auto
	return writeJSON(p.state, s, 0o644)
}

// take makes every build Kiri's: the alias and the global current builder,
// after keeping what was there. False when Kiri's builder is not made yet;
// apply takes it once it is.
func (p buildPlaces) take() (bool, error) {
	if !p.ready() {
		return false, nil
	}
	s := p.readState()
	if s["before"] == nil {
		cfg, err := readJSON(p.config())
		if err != nil {
			return false, err
		}
		var current any
		if raw, err := os.ReadFile(p.current()); err == nil {
			current = string(raw)
		} else if !errors.Is(err, os.ErrNotExist) {
			return false, err
		}
		aliases, _ := cfg["aliases"].(object)
		_, had := cfg["aliases"]
		s["before"] = object{"alias": aliases["builder"], "aliases": had, "current": current}
		if err := writeJSON(p.state, s, 0o644); err != nil {
			return false, err
		}
	}
	cfg, err := readJSON(p.config())
	if err != nil {
		return false, err
	}
	aliases, ok := cfg["aliases"].(object)
	if !ok {
		aliases = object{}
		cfg["aliases"] = aliases
	}
	if aliases["builder"] != "buildx" {
		aliases["builder"] = "buildx"
		if err := writeJSON(p.config(), cfg, 0o600); err != nil {
			return false, err
		}
	}
	// buildx's own format, as `docker buildx use --global` writes it.
	raw, err := encodeJSON(object{"Key": p.context, "Name": p.builder, "Global": true}, false)
	if err != nil {
		return false, err
	}
	return true, writeFile(p.current(), raw, 0o600)
}

// giveBack puts back what take kept, where it is still as Kiri left it.
func (p buildPlaces) giveBack() error {
	s := p.readState()
	before, kept := s["before"].(object)
	if !kept {
		return nil
	}
	cfg, err := readJSON(p.config())
	if err != nil {
		return err
	}
	if aliases, _ := cfg["aliases"].(object); aliases["builder"] == "buildx" {
		if before["alias"] == nil {
			delete(aliases, "builder")
			if before["aliases"] != true && len(aliases) == 0 {
				delete(cfg, "aliases")
			}
		} else {
			aliases["builder"] = before["alias"]
		}
		if err := writeJSON(p.config(), cfg, 0o600); err != nil {
			return err
		}
	}
	if cur, _ := readJSON(p.current()); cur["Name"] == p.builder && cur["Global"] == true {
		if was, ok := before["current"].(string); ok {
			if err := writeFile(p.current(), []byte(was), 0o600); err != nil {
				return err
			}
		} else if err := os.Remove(p.current()); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	delete(s, "before")
	return writeJSON(p.state, s, 0o644)
}

// apply makes what was chosen so, whenever Kiri's builder has been made. It
// never fights a change made since, such as a `docker buildx use` of the
// user's own: only choosing again does that. With nothing chosen and no
// other engine on the machine, auto lets Kiri take the builds unasked.
func (p buildPlaces) apply(other string, auto bool) error {
	if err := p.shadow(); err != nil {
		return err
	}
	switch saved := p.saved(); {
	case saved == "kiri" && p.readState()["before"] == nil:
		_, err := p.take()
		return err
	case saved == "docker":
		return p.giveBack()
	case saved == "" && other == "" && auto:
		if err := p.save("kiri", true); err != nil {
			return err
		}
		_, err := p.take()
		return err
	}
	return nil
}

// choose keeps the user's choice and makes it so now.
func (p buildPlaces) choose(with string) error {
	if err := p.save(with, false); err != nil {
		return err
	}
	if err := p.shadow(); err != nil {
		return err
	}
	if with == "kiri" {
		_, err := p.take()
		return err
	}
	return p.giveBack()
}

// uninstall gives Docker's builder back and removes the copy and build.json.
func (p buildPlaces) uninstall() error {
	if err := p.giveBack(); err != nil {
		return err
	}
	path := p.instance(p.context)
	if shadow, err := readJSON(path); err != nil {
		return err
	} else if p.copied(shadow) {
		if err := os.Remove(path); err != nil {
			return err
		}
	}
	if err := os.Remove(p.state); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// places locates the build files for this installation.
func (s *setup) places() buildPlaces {
	buildx := os.Getenv("BUILDX_CONFIG")
	if buildx == "" {
		buildx = filepath.Join(s.docker, "buildx")
	}
	return buildPlaces{
		docker: s.docker, buildx: buildx, state: filepath.Join(s.in.root(), "build.json"),
		builder: s.in.names.builder, context: s.in.names.context,
	}
}

// otherEngine names the Docker engine besides this one on the Mac, if there
// is one: serving now, or installed though not running.
func (s *setup) otherEngine() string {
	ours, _ := filepath.EvalSymlinks(s.in.socket())
	home := s.in.home
	for _, socket := range []string{
		filepath.Join(home, ".orbstack/run/docker.sock"),
		filepath.Join(home, ".docker/run/docker.sock"),
		filepath.Join(home, ".docker/desktop/docker.sock"),
		filepath.Join(home, ".rd/docker.sock"),
		filepath.Join(home, ".colima/default/docker.sock"),
		s.root + dockerSock,
	} {
		// A /var/run/docker.sock that points at this engine is not another one.
		target, err := filepath.EvalSymlinks(socket)
		if err != nil || target == ours {
			continue
		}
		switch {
		case strings.Contains(target, ".orbstack"):
			return "OrbStack"
		case strings.Contains(target, "/.docker/"):
			return "Docker Desktop"
		case strings.Contains(target, "/.rd/"):
			return "Rancher Desktop"
		case strings.Contains(target, "/.colima/"):
			return "Colima"
		case strings.Contains(target, "podman"):
			return "Podman"
		}
		return "Docker"
	}
	for _, app := range []string{"OrbStack", "Docker", "Rancher Desktop"} {
		if exists(s.root + "/Applications/" + app + ".app") {
			if app == "Docker" {
				return "Docker Desktop"
			}
			return app
		}
	}
	return ""
}

// buildWith is `kiri build-with [docker|kiri]`: says which, or switches.
func (s *setup) buildWith(args []string) error {
	p, other := s.places(), s.otherEngine()
	if len(args) > 0 {
		if len(args) != 1 || (args[0] != "docker" && args[0] != "kiri") {
			return errors.New("usage: kiri build-with [docker|kiri]")
		}
		if err := p.choose(args[0]); err != nil {
			return err
		}
		if args[0] == "kiri" && !p.ready() {
			_, _ = s.ensureBuilder()
		}
	}
	with := p.saved()
	if with == "" {
		// Not chosen yet: what apply does by itself on this machine.
		with = "docker"
		if other == "" && !s.in.names.flavored {
			with = "kiri"
		}
	}
	switch {
	case with == "kiri" && !p.ready():
		fmt.Fprintf(s.out, "docker build builds on Kiri once its builder `%s` is made (`kiri setup` makes it while Kiri runs)\n", p.builder)
	case with == "kiri" && other != "":
		fmt.Fprintf(s.out, "docker build and docker compose build run on Kiri, whatever docker context is in use; the images are in Kiri, not %s. `kiri build-with docker` switches back\n", other)
	case with == "kiri":
		fmt.Fprintf(s.out, "docker build and docker compose build run on Kiri (builder `%s`)\n", p.builder)
	default:
		theirs := ""
		if other != "" {
			theirs = fmt.Sprintf(" (%s's builds stay %s's)", other, other)
		}
		fmt.Fprintf(s.out, "docker build follows docker's context%s: on Kiri when docker uses Kiri. `kiri build-with kiri` sends every build to Kiri\n", theirs)
	}
	return nil
}
