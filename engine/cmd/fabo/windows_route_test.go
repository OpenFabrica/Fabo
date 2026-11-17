//go:build linux || darwin

package main

import (
	"reflect"
	"strings"
	"testing"
)

// TestRouteWindows sends each kind of command where the Windows fabo runs it.
func TestRouteWindows(t *testing.T) {
	for _, c := range []struct {
		args []string
		want windowsRoute
	}{
		{nil, routeWindow},
		{[]string{"ui"}, routeWindow},
		{[]string{"--version"}, routeVersion},
		{[]string{"run", "--rm", "alpine"}, routeDocker},
		{[]string{"compose", "up"}, routeDocker},
		{[]string{"update", "--restart", "always", "web"}, routeDocker},
		{[]string{"k8s", "start"}, routeWSL},
		{[]string{"machine", "list"}, routeWSL},
		{[]string{"machine", "shell", "dev"}, routeShell},
		{[]string{"engine", "restart"}, routeEngine},
		{[]string{"engine", "explode"}, routeUsage},
		{[]string{"setup"}, routeUnsupported},
		{[]string{"context", "use", "fabo"}, routeUnsupported},
		{[]string{"frobnicate"}, routeUsage},
	} {
		if got := routeWindows(c.args); got != c.want {
			t.Errorf("routeWindows(%q) = %d, want %d", c.args, got, c.want)
		}
	}
}

// TestWindowsArgs checks the command lines the Windows fabo builds.
func TestWindowsArgs(t *testing.T) {
	got := wslArgs("fabo", []string{"k8s", "status"})
	if !reflect.DeepEqual(got[:6], []string{"-d", "fabo", "-u", "root", "-e", "sh"}) || got[8] != "fabo" || !reflect.DeepEqual(got[9:], []string{"k8s", "status"}) {
		t.Fatalf("wslArgs = %q", got)
	}
	if !strings.Contains(got[7], `exec /usr/local/bin/fabo "$@"`) {
		t.Fatalf("script = %q", got[7])
	}
	if got := windowsShellArgs("dev", []string{"--", "uname", "-a"}, false); !reflect.DeepEqual(got[len(got)-3:], []string{"machine-dev", "uname", "-a"}) || got[1] != "-i" {
		t.Fatalf("shell with command = %q", got)
	}
	if got := windowsShellArgs("dev", nil, true); got[1] != "-it" || got[len(got)-3] != "sh" {
		t.Fatalf("login shell = %q", got)
	}
	// Every docker command the launcher passes through goes through here too.
	for _, c := range strings.Fields("run ps exec compose buildx logs volume network system") {
		if !handlesDocker(c) || !windowsDocker(c) {
			t.Errorf("%s is not a docker command on both", c)
		}
	}
}
