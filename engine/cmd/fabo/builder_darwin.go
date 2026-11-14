package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// dockerCLI is the docker CLI to drive buildx with: on PATH, else the app's.
func (s *setup) dockerCLI() string {
	return s.in.dockerCLI()
}

// ensureBuilder registers buildx's docker-container builder on this
// installation's context, loading every build into the engine. Its BuildKit
// is the daemon's managed builder: the daemon makes that container when
// buildx first looks for it, starts it on demand and stops it when idle, so
// this only registers the name with buildx and applies the build-with
// choice. False when there is no docker CLI or buildx plugin to do it with:
// `docker build` then sends the classic POST /build, which the daemon runs
// in the same BuildKit.
func (s *setup) ensureBuilder() (bool, error) {
	docker := s.dockerCLI()
	if docker == "" {
		return false, nil
	}
	// buildx talks to the daemon, and can wait on it for good: give a
	// daemon that was just started a few seconds, else skip.
	client := socketClient(s.in.socket())
	defer client.CloseIdleConnections()
	for deadline := time.Now().Add(5 * time.Second); !alive(client); time.Sleep(200 * time.Millisecond) {
		if time.Now().After(deadline) {
			return false, errors.New("the Fabo daemon isn't answering yet")
		}
	}
	builder, context := s.in.names.builder, s.in.names.context
	if exec.Command(docker, "buildx", "version").Run() != nil {
		fmt.Fprintln(s.out, "docker build: no buildx plugin here, so docker builds with Fabo's classic builder API (the same BuildKit, run by the daemon)")
		return false, nil
	}
	inspect, err := exec.Command(docker, "buildx", "inspect", builder).Output()
	made := err == nil
	// Without default-load a docker-container builder keeps what it builds
	// to itself: `docker build -t x .` builds and `docker run x` finds
	// nothing. One made without it is made again; --keep-state keeps its
	// cache volume, which the daemon's builder container reuses.
	if made && !strings.Contains(string(inspect), "default-load") {
		made = exec.Command(docker, "buildx", "rm", "--keep-state", builder).Run() != nil
	}
	if !made {
		// buildx create takes the context's name; without that context
		// buildx reads the name as a host and fails on tcp://fabo:2375.
		if !isFile(filepath.Join(s.contextDir(context), "meta.json")) {
			if err := s.writeContext(); err != nil {
				return false, err
			}
		}
		create := exec.Command(docker, "buildx", "create", "--name", builder, "--driver", "docker-container", "--driver-opt", "default-load=true", context)
		var stderr strings.Builder
		create.Stderr = &stderr
		if create.Run() != nil {
			return false, errors.New(lastLine("docker buildx create failed", stderr.String()))
		}
	}
	// Which builder plain `docker build` uses needs this builder to exist.
	// A flavored app never takes the machine's builds unasked: it runs
	// beside an installed app, which is the one that may.
	if err := s.places().apply(s.otherEngine(), !s.in.names.flavored); err != nil {
		fmt.Fprintf(s.out, "docker build: could not set which builder it uses (%v)\n", err)
	}
	return true, nil
}

// lastLine is what failed, with the tool's own last error line when it gave one.
func lastLine(what, stderr string) string {
	lines := strings.Split(strings.TrimSpace(stderr), "\n")
	if last := strings.TrimSpace(lines[len(lines)-1]); last != "" {
		return what + ": " + strings.TrimPrefix(last, "ERROR: ")
	}
	return what
}

// removeBuilder drops the buildx builder. By the time uninstall calls this
// the engine is stopped, and buildx can fail on a builder it cannot reach,
// so the builder's record is removed directly as well.
func (s *setup) removeBuilder() {
	if docker := s.dockerCLI(); docker != "" {
		cmd := exec.Command(docker, "buildx", "rm", s.in.names.builder)
		cmd.Stdout, cmd.Stderr = nil, nil
		_ = cmd.Run()
	}
	_ = os.Remove(s.places().instance(s.in.names.builder))
}
