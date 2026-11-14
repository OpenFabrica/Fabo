package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// contextState is which engine plain docker uses, as `fabo context` reports
// it to the desktop: the current context, this installation's own, and the
// app's name.
type contextState struct {
	Current string `json:"current"`
	Ours    string `json:"ours"`
	App     string `json:"app"`
}

// current is the context plain docker uses: DOCKER_CONTEXT when set, else
// docker's config, else "default".
func (s *setup) current() string {
	if name := os.Getenv("DOCKER_CONTEXT"); name != "" {
		return name
	}
	cfg, _ := readJSON(filepath.Join(s.docker, "config.json"))
	if name, _ := cfg["currentContext"].(string); name != "" {
		return name
	}
	return "default"
}

// switchContext makes plain docker use the named context. This
// installation's own is written first, so switching to it works even before
// `fabo setup`; "default" clears the choice; any other context must exist.
func (s *setup) switchContext(name string) error {
	switch {
	case name == s.in.names.context:
		if err := s.writeContext(); err != nil {
			return err
		}
	case name == "default":
	default:
		if _, err := os.Stat(filepath.Join(s.contextDir(name), "meta.json")); err != nil {
			return fmt.Errorf("there is no docker context %q", name)
		}
	}
	path := filepath.Join(s.docker, "config.json")
	cfg, err := readJSON(path)
	if err != nil {
		return err
	}
	if name == "default" {
		delete(cfg, "currentContext")
	} else {
		cfg["currentContext"] = name
	}
	return writeJSON(path, cfg, 0o600)
}

// contextCommand is `fabo context [use NAME]`: report or change which
// engine plain docker uses. The desktop offers it when docker still points
// at OrbStack or Docker Desktop.
func contextCommand(args []string) error {
	in, err := locate()
	if err != nil {
		return err
	}
	s := newSetup(in)
	switch {
	case len(args) == 0:
		current := s.current()
		// docker's default endpoint is /var/run/docker.sock; when setup
		// linked it to Fabo, the default context is Fabo too.
		if owner, _ := s.dockerSock(); current == "default" && owner == sockOurs {
			current = in.names.context
		}
		return json.NewEncoder(os.Stdout).Encode(contextState{current, in.names.context, in.names.app})
	case len(args) == 2 && args[0] == "use" && args[1] != "":
		return s.switchContext(args[1])
	}
	return errors.New("usage: fabo context [use NAME]")
}
