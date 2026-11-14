package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// contextDir is the docker CLI's directory for a context, named by the
// SHA-256 of the context's name.
func (s *setup) contextDir(name string) string {
	sum := sha256.Sum256([]byte(name))
	return filepath.Join(s.docker, "contexts", "meta", hex.EncodeToString(sum[:]))
}

// writeContext writes this installation's context, as
// `docker context create fabo --docker host=…` would.
func (s *setup) writeContext() error {
	n := s.in.names
	return writeJSON(filepath.Join(s.contextDir(n.context), "meta.json"), object{
		"Name":     n.context,
		"Metadata": object{"Description": n.app},
		"Endpoints": object{"docker": object{
			"Host": "unix://" + s.in.socket(), "SkipTLSVerify": false,
		}},
	}, 0o644)
}

// useContext makes this installation's context the one plain docker uses.
func (s *setup) useContext() error {
	path := filepath.Join(s.docker, "config.json")
	cfg, err := readJSON(path)
	if err != nil {
		return err
	}
	cfg["currentContext"] = s.in.names.context
	return writeJSON(path, cfg, 0o600)
}

// defaultContext makes plain docker use Fabo on a Mac with no other Docker
// engine. With OrbStack or Docker Desktop around, or a context the user
// already picked, the choice stays as it is.
func (s *setup) defaultContext() error {
	// Fabo's own /var/run/docker.sock is not another engine.
	owner, _ := s.dockerSock()
	other := (owner != sockOurs && exists(s.root+dockerSock)) ||
		exists(filepath.Join(s.in.home, ".orbstack/run/docker.sock")) ||
		exists(filepath.Join(s.in.home, ".docker/run/docker.sock")) ||
		exists(s.root+"/Applications/OrbStack.app") ||
		exists(s.root+"/Applications/Docker.app")
	path := filepath.Join(s.docker, "config.json")
	cfg, err := readJSON(path)
	if err != nil {
		return err
	}
	if current, _ := cfg["currentContext"].(string); other || (current != "" && current != "default") {
		return nil
	}
	cfg["currentContext"] = s.in.names.context
	if err := writeJSON(path, cfg, 0o600); err != nil {
		return err
	}
	fmt.Fprintln(s.out, "docker uses Fabo by default (no other Docker engine on this Mac)")
	return nil
}

// addPluginDir adds the app's Compose and buildx to the docker CLI's plugin
// search path, once.
func (s *setup) addPluginDir(dir string) error {
	path := filepath.Join(s.docker, "config.json")
	cfg, err := readJSON(path)
	if err != nil {
		return err
	}
	dirs, _ := cfg["cliPluginsExtraDirs"].([]any)
	for _, d := range dirs {
		if d == dir {
			return nil
		}
	}
	cfg["cliPluginsExtraDirs"] = append(dirs, dir)
	return writeJSON(path, cfg, 0o600)
}

// forgetDocker takes back what setup wrote for the docker CLI: the context
// if it still points at this installation's socket, the current-context
// choice if it is still this one, and this app's plugin directory.
func (s *setup) forgetDocker() error {
	n := s.in.names
	dir := s.contextDir(n.context)
	meta, err := readJSON(filepath.Join(dir, "meta.json"))
	if err != nil {
		return err
	}
	endpoints, _ := meta["Endpoints"].(object)
	docker, _ := endpoints["docker"].(object)
	if host, _ := docker["Host"].(string); strings.HasSuffix(host, n.state+"/run/docker.sock") {
		if err := os.RemoveAll(dir); err != nil {
			return err
		}
		fmt.Fprintf(s.out, "removed docker context `%s`\n", n.context)
	}
	path := filepath.Join(s.docker, "config.json")
	cfg, err := readJSON(path)
	if err != nil {
		return err
	}
	changed := false
	if cfg["currentContext"] == n.context {
		delete(cfg, "currentContext")
		changed = true
	}
	// Only the directory this installation added: another copy of the app
	// has its own entry here, and docker finds Compose and buildx through it.
	if dirs, ok := cfg["cliPluginsExtraDirs"].([]any); ok && !n.flavored && s.contents() != "" {
		ours := filepath.Join(s.contents(), "Resources", "bin", "cli-plugins")
		kept := []any{}
		for _, d := range dirs {
			if d != ours {
				kept = append(kept, d)
			}
		}
		if len(kept) != len(dirs) {
			cfg["cliPluginsExtraDirs"], changed = kept, true
		}
	}
	if !changed {
		return nil
	}
	return writeJSON(path, cfg, 0o600)
}
