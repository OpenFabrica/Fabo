package main

import (
	"path/filepath"
	"testing"
)

// Switching plain docker: to Fabo writes Fabo's context first, to another
// engine needs that context to exist, and "default" clears the choice.
// DOCKER_CONTEXT, when set, is what docker uses.
func TestContextSwitchAndReport(t *testing.T) {
	s := sandbox(t, appID)
	t.Setenv("DOCKER_CONTEXT", "")
	if got := s.current(); got != "default" {
		t.Fatal(got)
	}
	if err := s.switchContext("orbstack"); err == nil {
		t.Fatal("switched to a context that does not exist")
	}
	write(t, filepath.Join(s.contextDir("orbstack"), "meta.json"), `{"Name":"orbstack"}`)
	if err := s.switchContext("orbstack"); err != nil || s.current() != "orbstack" {
		t.Fatal(err, s.current())
	}
	if err := s.switchContext("fabo"); err != nil || s.current() != "fabo" {
		t.Fatal(err, s.current())
	}
	if meta, _ := readJSON(filepath.Join(s.contextDir("fabo"), "meta.json")); meta["Name"] != "fabo" {
		t.Fatal(meta)
	}
	if err := s.switchContext("default"); err != nil || s.current() != "default" {
		t.Fatal(err, s.current())
	}
	if cfg, _ := readJSON(filepath.Join(s.docker, "config.json")); cfg["currentContext"] != nil {
		t.Fatal(cfg)
	}
	t.Setenv("DOCKER_CONTEXT", "desktop-linux")
	if got := s.current(); got != "desktop-linux" {
		t.Fatal(got)
	}
}
