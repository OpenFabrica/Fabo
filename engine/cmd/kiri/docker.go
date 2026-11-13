//go:build linux || darwin

package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
)

// import belongs to Kiri's migration command. On macOS update belongs to
// the application updater; container updates use `fabo container update`.
func handlesDocker(command string) bool {
	if command == "update" && runtime.GOOS == "darwin" {
		return false
	}
	return slices.Contains(strings.Fields(`attach build builder buildx commit compose container cp create diff events exec export history image images
info inspect kill load login logout logs manifest network pause port ps pull push rename restart rm rmi
run save search start stats stop system tag top unpause update version volume wait`), command)
}

// runDocker replaces the launcher so terminal I/O, signals and exit status
// remain Docker's. It never starts an engine or changes the user's context.
func (i installation) runDocker(args []string) error {
	docker := i.dockerCLI()
	if docker == "" {
		return errors.New("no docker CLI found; open Fabo once to set it up")
	}
	client := socketClient(i.socket())
	running := alive(client)
	client.CloseIdleConnections()
	if !running {
		return errors.New("Fabo isn't running. Open Fabo, or start it with `fabo daemon`")
	}
	return syscall.Exec(docker, append([]string{docker}, args...), i.dockerEnvironment())
}

// dockerCLI prefers PATH, then the CLI shipped in this app bundle.
func (i installation) dockerCLI() string {
	if docker, err := exec.LookPath("docker"); err == nil {
		return docker
	}
	docker := filepath.Join(i.dir, "..", "Resources", "bin", "docker")
	if info, err := os.Stat(docker); err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0 {
		return docker
	}
	return ""
}

// dockerEnvironment pins the endpoint even when another context is active.
// A user's explicit builder (including an empty value) takes precedence.
func (i installation) dockerEnvironment() []string {
	env := slices.DeleteFunc(os.Environ(), func(value string) bool {
		return strings.HasPrefix(value, "DOCKER_HOST=") || strings.HasPrefix(value, "DOCKER_CONTEXT=")
	})
	env = append(env, "DOCKER_HOST=unix://"+i.socket())
	if _, set := os.LookupEnv("BUILDX_BUILDER"); !set && i.buildxKnows() {
		env = append(env, "BUILDX_BUILDER="+i.names.builder)
	}
	return env
}

func (i installation) buildxKnows() bool {
	dir, set := os.LookupEnv("BUILDX_CONFIG")
	if !set {
		config, set := os.LookupEnv("DOCKER_CONFIG")
		if !set {
			config = filepath.Join(i.home, ".docker")
		}
		dir = filepath.Join(config, "buildx")
	}
	info, err := os.Stat(filepath.Join(dir, "instances", i.names.builder))
	return err == nil && info.Mode().IsRegular()
}
