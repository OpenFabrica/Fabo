package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// dockerSock is where tools that ignore docker contexts look for Docker:
// docker-py, Go's client.FromEnv, docker-java, Testcontainers, IDE plugins.
const dockerSock = "/var/run/docker.sock"

// sockOwner is who has /var/run/docker.sock.
type sockOwner int

const (
	sockFree sockOwner = iota
	sockOurs
	sockTaken // another engine's
)

// dockerSock classifies /var/run/docker.sock. The second result is what
// another engine's link points at.
func (s *setup) dockerSock() (sockOwner, string) {
	// These engines' links stay theirs while the app is installed, running
	// or not: their socket is missing only until they start.
	var engines []string
	for app, dir := range map[string]string{"Docker": ".docker", "OrbStack": ".orbstack", "Rancher Desktop": ".rd"} {
		if exists(s.root + "/Applications/" + app + ".app") {
			engines = append(engines, filepath.Join(s.in.home, dir))
		}
	}
	return dockerSockAt(s.root+dockerSock, s.in.socket(), engines)
}

// dockerSockAt classifies path given this installation's socket and the
// directories of the other engines installed.
func dockerSockAt(path, ours string, engines []string) (sockOwner, string) {
	info, err := os.Lstat(path)
	if err != nil {
		return sockFree, ""
	}
	if info.Mode()&os.ModeSymlink == 0 {
		// A socket (or anything else) of its own: an engine's, serving or not.
		return sockTaken, path
	}
	target, err := os.Readlink(path)
	if err != nil {
		return sockTaken, path
	}
	if target == ours {
		return sockOurs, ""
	}
	if exists(target) {
		return sockTaken, target
	}
	for _, engine := range engines {
		if target == engine || strings.HasPrefix(target, engine+"/") {
			return sockTaken, target
		}
	}
	// Left behind by an engine that has been removed.
	return sockFree, ""
}

// testcontainersMark is the first line of the ~/.testcontainers.properties
// Fabo writes. Only a file that starts with it is Fabo's to change or remove.
const testcontainersMark = "# Written by Fabo while /var/run/docker.sock is not Fabo's (fabo setup --uninstall removes it)"

// testcontainers writes (on) or removes Fabo's ~/.testcontainers.properties,
// which points Testcontainers at this engine. A file the user or
// Testcontainers itself wrote is never touched. True when Fabo's file is
// there afterwards.
func testcontainers(path, ours string, on bool) (bool, error) {
	want := testcontainersMark + "\ndocker.host=unix://" + ours + "\n"
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		if on {
			return true, os.WriteFile(path, []byte(want), 0o644)
		}
		return false, nil
	}
	if err != nil {
		return false, err
	}
	text := string(raw)
	if !strings.HasPrefix(text, testcontainersMark) {
		return false, nil
	}
	// Only exactly what Fabo wrote: Testcontainers adds keys of its own.
	for _, line := range strings.Split(text, "\n") {
		if strings.TrimSpace(line) != "" && !strings.HasPrefix(line, "#") && !strings.HasPrefix(line, "docker.host=") {
			return false, nil
		}
	}
	if !on {
		return false, os.Remove(path)
	}
	if text != want {
		return true, os.WriteFile(path, []byte(want), 0o644)
	}
	return true, nil
}

// socketNote says where tools that ignore docker contexts find Fabo, and
// keeps ~/.testcontainers.properties for as long as /var/run/docker.sock is
// free but not Fabo's.
func (s *setup) socketNote() string {
	ours := s.in.socket()
	properties := filepath.Join(s.in.home, ".testcontainers.properties")
	switch owner, target := s.dockerSock(); owner {
	case sockOurs:
		_, _ = testcontainers(properties, ours, false)
		return dockerSock + " → Fabo (docker-py, Testcontainers and other tools that ignore docker contexts)"
	case sockTaken:
		_, _ = testcontainers(properties, ours, false)
		return fmt.Sprintf("%s is another engine's (%s) and stays so; DOCKER_HOST=unix://%s points a tool at Fabo", dockerSock, target, ours)
	}
	if written, err := testcontainers(properties, ours, true); err == nil && written {
		return dockerSock + " is not Fabo's; Testcontainers finds Fabo through ~/.testcontainers.properties"
	}
	return fmt.Sprintf("%s is not Fabo's; DOCKER_HOST=unix://%s points a tool at Fabo", dockerSock, ours)
}
