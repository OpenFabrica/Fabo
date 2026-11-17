package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/OpenFabrica/Fabo/engine/internal/buildwith"
)

// Which builder plain `docker build` uses, Docker's or Fabo's: the logic,
// shared with fabo-win on Windows, is internal/buildwith. Here are only the
// Mac's places, its other engines and the `fabo build-with` command.

// places locates the build files for this installation.
func (s *setup) places() buildwith.Places {
	buildx := os.Getenv("BUILDX_CONFIG")
	if buildx == "" {
		buildx = filepath.Join(s.docker, "buildx")
	}
	return buildwith.Places{
		Docker: s.docker, Buildx: buildx, State: filepath.Join(s.in.root(), "build.json"),
		Builder: s.in.names.builder, Context: s.in.names.context,
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

// buildWith is `fabo build-with [docker|fabo]`: says which, or switches.
func (s *setup) buildWith(args []string) error {
	// Keep the persisted value "fabo" so existing builder choices still apply.
	if len(args) == 1 && args[0] == "fabo" {
		args = []string{"fabo"}
	}
	p, other := s.places(), s.otherEngine()
	if len(args) > 0 {
		if len(args) != 1 || (args[0] != "docker" && args[0] != "fabo") {
			return errors.New("usage: fabo build-with [docker|fabo]")
		}
		if err := p.Choose(args[0]); err != nil {
			return err
		}
		if args[0] == "fabo" && !p.Ready() {
			_, _ = s.ensureBuilder()
		}
	}
	with := p.Saved()
	if with == "" {
		// Not chosen yet: what apply does by itself on this machine.
		with = "docker"
		if other == "" && !s.in.names.flavored {
			with = "fabo"
		}
	}
	switch {
	case with == "fabo" && !p.Ready():
		fmt.Fprintf(s.out, "docker build builds on Fabo once its builder `%s` is made (`fabo setup` makes it while Fabo runs)\n", p.Builder)
	case with == "fabo" && other != "":
		fmt.Fprintf(s.out, "docker build and docker compose build run on Fabo, whatever docker context is in use; the images are in Fabo, not %s. `fabo build-with docker` switches back\n", other)
	case with == "fabo":
		fmt.Fprintf(s.out, "docker build and docker compose build run on Fabo (builder `%s`)\n", p.Builder)
	default:
		theirs := ""
		if other != "" {
			theirs = fmt.Sprintf(" (%s's builds stay %s's)", other, other)
		}
		fmt.Fprintf(s.out, "docker build follows docker's context%s: on Fabo when docker uses Fabo. `fabo build-with fabo` sends every build to Fabo\n", theirs)
	}
	return nil
}
