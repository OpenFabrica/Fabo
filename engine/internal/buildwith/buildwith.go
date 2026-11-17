// Package buildwith decides which builder plain `docker build` uses:
// Docker's own, or Fabo's. The macOS launcher (`fabo build-with`, setup)
// and fabo-win on Windows (Settings → Builds, uninstall) share it. Upstream
// Kiri's crates/kiri/src/buildwith.rs.
//
// The docker CLI hands `docker build` to buildx with BUILDX_BUILDER set to
// the current context, unless the config's `builder` alias is set (what
// `docker buildx install` does), in which case buildx takes its current
// builder (<buildx>/current). Compose and `docker buildx build` always take
// buildx's current builder.
//
//   - Always: a buildx builder named after Fabo's context, a copy of Fabo's
//     builder. A builder in buildx's store wins over the context of the same
//     name, so `docker --context fabo build` builds on Fabo and loads into
//     Fabo. No other context is touched.
//   - Build with Docker: nothing more. Builds follow docker's context.
//   - Build with Fabo: the alias, and Fabo's builder as buildx's global
//     current builder. Every build runs on Fabo whatever context is in use.
//
// What "Build with Fabo" changes is kept in build.json first, and choosing
// Docker puts it back exactly, unless something else has changed it since.
package buildwith

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/OpenFabrica/Fabo/engine/internal/jsonfile"
)

// The two choices, as build.json and the control API spell them.
const (
	Docker = "docker"
	Fabo   = "fabo"
)

// object is a JSON object as docker and buildx files hold it.
type object = jsonfile.Object

// Places is where everything is: docker's config directory, buildx's,
// Fabo's build.json, and Fabo's builder and context names.
type Places struct {
	Docker, Buildx, State string
	Builder, Context      string
}

// Status is what the window shows about the choice.
type Status struct {
	// With is "docker" or "fabo": the saved choice, else what happens
	// until someone chooses.
	With string `json:"with"`
	// Ask: another engine is here and nobody has chosen yet.
	Ask bool `json:"ask"`
	// Engine names the other engine, if any.
	Engine string `json:"engine,omitempty"`
	// Taken: every build goes to Fabo's builder right now.
	Taken bool `json:"taken"`
	// Ready: Fabo's builder is in buildx's store.
	Ready   bool   `json:"ready"`
	Builder string `json:"builder"`
	Context string `json:"context"`
	Error   string `json:"error,omitempty"`
}

// DockerDirs is docker's config directory and buildx's, as the docker CLI
// finds them: DOCKER_CONFIG, else <home>/.docker; BUILDX_CONFIG, else
// <docker>/buildx.
func DockerDirs(home string, getenv func(string) string) (docker, buildx string) {
	if docker = getenv("DOCKER_CONFIG"); docker == "" {
		docker = filepath.Join(home, ".docker")
	}
	if buildx = getenv("BUILDX_CONFIG"); buildx == "" {
		buildx = filepath.Join(docker, "buildx")
	}
	return docker, buildx
}

// Config is docker's config.json.
func (p Places) Config() string { return filepath.Join(p.Docker, "config.json") }

// Current is buildx's record of its current builder.
func (p Places) Current() string { return filepath.Join(p.Buildx, "current") }

// Instance is buildx's record of the builder called name.
func (p Places) Instance(name string) string {
	return filepath.Join(p.Buildx, "instances", name)
}

// readState is build.json; unreadable counts as empty.
func (p Places) readState() object {
	s, err := jsonfile.Read(p.State)
	if err != nil {
		return object{}
	}
	return s
}

// Saved is what was chosen, Docker or Fabo, or "" when nobody has.
func (p Places) Saved() string {
	if with, _ := p.readState()["with"].(string); with == Docker || with == Fabo {
		return with
	}
	return ""
}

// Ask reports whether to ask: another engine is here and nobody has
// chosen. A choice Fabo made by itself on a machine that had no other
// engine at the time is asked about once one appears.
func (p Places) Ask(other string) bool {
	s := p.readState()
	return other != "" && (p.Saved() == "" || s["auto"] == true)
}

// Ready reports whether Fabo's builder is in buildx's store yet.
func (p Places) Ready() bool { return jsonfile.IsFile(p.Instance(p.Builder)) }

// Taken reports whether `docker build` goes to Fabo's builder in every
// context now.
func (p Places) Taken() bool {
	cfg, _ := jsonfile.Read(p.Config())
	cur, _ := jsonfile.Read(p.Current())
	aliases, _ := cfg["aliases"].(object)
	return aliases["builder"] == "buildx" && cur["Name"] == p.Builder && cur["Global"] == true
}

// shadow keeps the builder named after Fabo's context a copy of Fabo's
// builder. One of that name that is not such a copy is left alone.
func (p Places) shadow() error {
	ours, err := jsonfile.Read(p.Instance(p.Builder))
	if err != nil {
		return err
	}
	if _, made := ours["Name"]; !made {
		return nil
	}
	path := p.Instance(p.Context)
	theirs, err := jsonfile.Read(path)
	if err != nil {
		return err
	}
	if _, named := theirs["Name"]; named && !p.Copied(theirs) {
		return nil
	}
	ours["Name"] = p.Context
	if reflect.DeepEqual(theirs, ours) {
		return nil
	}
	raw, err := jsonfile.Encode(ours, false)
	if err != nil {
		return err
	}
	return jsonfile.WriteFile(path, raw, 0o600)
}

// Copied reports whether a builder is Fabo's copy: its nodes are Fabo's builder's.
func (p Places) Copied(instance object) bool {
	nodes, _ := instance["Nodes"].([]any)
	for _, node := range nodes {
		n, _ := node.(object)
		if name, _ := n["Name"].(string); !strings.HasPrefix(name, p.Builder) {
			return false
		}
	}
	return len(nodes) > 0
}

// save records the choice. auto marks one made without asking.
func (p Places) save(with string, auto bool) error {
	s := p.readState()
	s["with"], s["auto"] = with, auto
	return jsonfile.Write(p.State, s, 0o644)
}

// take makes every build Fabo's: the alias and the global current builder,
// after keeping what was there. False when Fabo's builder is not made yet;
// Apply takes it once it is.
func (p Places) take() (bool, error) {
	if !p.Ready() {
		return false, nil
	}
	s := p.readState()
	if s["before"] == nil {
		cfg, err := jsonfile.Read(p.Config())
		if err != nil {
			return false, err
		}
		// The current builder is kept byte for byte, as buildx wrote it.
		var current any
		if raw, err := os.ReadFile(p.Current()); err == nil {
			current = string(raw)
		} else if !errors.Is(err, os.ErrNotExist) {
			return false, err
		}
		aliases, _ := cfg["aliases"].(object)
		_, had := cfg["aliases"]
		s["before"] = object{"alias": aliases["builder"], "aliases": had, "current": current}
		// The snapshot is on disk before anything it describes changes.
		if err := jsonfile.Write(p.State, s, 0o644); err != nil {
			return false, err
		}
	}
	cfg, err := jsonfile.Read(p.Config())
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
		if err := jsonfile.Write(p.Config(), cfg, 0o600); err != nil {
			return false, err
		}
	}
	// buildx's own format, as `docker buildx use --global` writes it.
	raw, err := jsonfile.Encode(object{"Key": p.Context, "Name": p.Builder, "Global": true}, false)
	if err != nil {
		return false, err
	}
	return true, jsonfile.WriteFile(p.Current(), raw, 0o600)
}

// GiveBack puts back what take kept, where it is still as Fabo left it.
// The choice itself stays, so Apply takes the builds again later: this is
// also what runs when Fabo's program is gone or Fabo quits.
func (p Places) GiveBack() error {
	s := p.readState()
	before, kept := s["before"].(object)
	if !kept {
		return nil
	}
	cfg, err := jsonfile.Read(p.Config())
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
		if err := jsonfile.Write(p.Config(), cfg, 0o600); err != nil {
			return err
		}
	}
	// A builder picked since (`docker buildx use`) is the user's: left alone.
	if cur, _ := jsonfile.Read(p.Current()); cur["Name"] == p.Builder && cur["Global"] == true {
		if was, ok := before["current"].(string); ok {
			if err := jsonfile.WriteFile(p.Current(), []byte(was), 0o600); err != nil {
				return err
			}
		} else if err := os.Remove(p.Current()); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	delete(s, "before")
	return jsonfile.Write(p.State, s, 0o644)
}

// Apply makes what was chosen so, whenever Fabo's builder has been made. It
// never fights a change made since, such as a `docker buildx use` of the
// user's own: only choosing again does that. With nothing chosen and no
// other engine on the machine, auto lets Fabo take the builds unasked.
func (p Places) Apply(other string, auto bool) error {
	if err := p.shadow(); err != nil {
		return err
	}
	switch saved := p.Saved(); {
	case saved == Fabo && p.readState()["before"] == nil:
		_, err := p.take()
		return err
	case saved == Docker:
		return p.GiveBack()
	case saved == "" && other == "" && auto:
		if err := p.save(Fabo, true); err != nil {
			return err
		}
		_, err := p.take()
		return err
	}
	return nil
}

// Choose keeps the user's choice and makes it so now.
func (p Places) Choose(with string) error {
	if with != Docker && with != Fabo {
		return errors.New(`build with "docker" or "fabo"`)
	}
	if err := p.save(with, false); err != nil {
		return err
	}
	if err := p.shadow(); err != nil {
		return err
	}
	if with == Fabo {
		_, err := p.take()
		return err
	}
	return p.GiveBack()
}

// Status is the choice as the window shows it. auto is Apply's: whether,
// with nothing chosen, Fabo takes the builds by itself.
func (p Places) Status(other string, auto bool) Status {
	with := p.Saved()
	if with == "" {
		// Not chosen yet: what Apply does by itself on this machine.
		with = Docker
		if other == "" && auto {
			with = Fabo
		}
	}
	return Status{
		With: with, Ask: p.Ask(other), Engine: other, Taken: p.Taken(), Ready: p.Ready(),
		Builder: p.Builder, Context: p.Context,
	}
}

// Uninstall gives Docker's builder back and removes the copy and build.json.
func (p Places) Uninstall() error {
	if err := p.GiveBack(); err != nil {
		return err
	}
	path := p.Instance(p.Context)
	if shadow, err := jsonfile.Read(path); err != nil {
		return err
	} else if p.Copied(shadow) {
		if err := os.Remove(path); err != nil {
			return err
		}
	}
	if err := os.Remove(p.State); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
