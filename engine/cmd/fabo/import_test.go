//go:build linux || darwin

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestImportOptionsAndContainerSpec(t *testing.T) {
	for _, args := range [][]string{{"unknown"}, {"orbstack", "--all"}, {"desktop-linux", "image"}, {"orbstack", "container", "-evil"}, {"orbstack", "--list", "extra"}} {
		if _, e := parseImport(args); e == nil {
			t.Fatal(args)
		}
	}
	o, e := parseImport([]string{"--list"})
	if e != nil || o.source != "orbstack" || !o.list {
		t.Fatal(o, e)
	}
	var info map[string]any
	if e = json.Unmarshal([]byte(`{"Id":"0123456789abcdef","Config":{"Image":"alpine:latest","Cmd":["sleep","123"],"Env":["A=B"],"Labels":{"keep":"yes"},"WorkingDir":"/work"},"HostConfig":{"Binds":["data:/data"],"Memory":1048576,"NetworkMode":"private","Privileged":false,"ExtraHosts":null},"NetworkSettings":{"Networks":{"private":{"Aliases":["0123456789ab","database"],"IPAddress":"10.2.0.4"}}}}`), &info); e != nil {
		t.Fatal(e)
	}
	spec := importContainerSpec(info)
	host := spec["HostConfig"].(map[string]any)
	if _, ok := host["ExtraHosts"]; ok {
		t.Fatal("kept null host option")
	}
	networks := spec["NetworkingConfig"].(map[string]any)["EndpointsConfig"].(map[string]any)
	ep := networks["private"].(map[string]any)
	if !reflect.DeepEqual(ep["Aliases"], []any{"database"}) || len(ep) != 1 || spec["Image"] != "alpine:latest" || host["Memory"] != float64(1048576) {
		t.Fatal(spec)
	}
	if !anonymousVolume(strings.Repeat("a1", 32)) || anonymousVolume("data") {
		t.Fatal("anonymous detection")
	}
}
func TestImportPipelineFailuresAndBackpressure(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if e := pipeImport(exec.CommandContext(ctx, "/bin/sh", "-c", "printf payload"), exec.CommandContext(ctx, "/bin/sh", "-c", `test "$(cat)" = payload`)); e != nil {
		t.Fatal(e)
	}
	if e := pipeImport(exec.CommandContext(ctx, "/bin/sh", "-c", "echo source-error >&2; exit 7"), exec.CommandContext(ctx, "/bin/cat")); e == nil || !strings.Contains(e.Error(), "source-error") {
		t.Fatal(e)
	}
	if e := pipeImport(exec.CommandContext(ctx, "/bin/sh", "-c", "while :; do echo long-output; done"), exec.CommandContext(ctx, "/bin/sh", "-c", "echo destination-error >&2; exit 8")); e == nil || !strings.Contains(e.Error(), "destination-error") {
		t.Fatal(e)
	}
	if e := pipeImport(exec.CommandContext(ctx, "/missing-command"), exec.CommandContext(ctx, "/bin/cat")); e == nil {
		t.Fatal("missing producer accepted")
	}
}
func TestImportEndpoints(t *testing.T) {
	t.Setenv("DOCKER_CONTEXT", "wrong")
	t.Setenv("DOCKER_HOST", "tcp://wrong")
	t.Setenv("FABO_SOCKET", "/private/target.sock")
	dir := t.TempDir()
	docker := filepath.Join(dir, "docker")
	if e := os.WriteFile(docker, []byte("#!/bin/sh\nprintf '%s\\n' \"${DOCKER_HOST-unset}\" \"${DOCKER_CONTEXT-unset}\" \"$@\"\n"), 0700); e != nil {
		t.Fatal(e)
	}
	i := importer{in: installation{home: dir, names: namesFor("")}, docker: docker, source: "orbstack", ctx: context.Background()}
	src, e := i.output(true, "save", "test")
	if e != nil || !strings.HasPrefix(string(src), "unset\nunset\n--context\norbstack\n") {
		t.Fatal(string(src), e)
	}
	dst, e := i.output(false, "load")
	if e != nil || !strings.HasPrefix(string(dst), "unix:///private/target.sock\nunset\nload") {
		t.Fatal(string(dst), e)
	}
}

func TestImportVolumeRollsBackFailedCopy(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "calls")
	t.Setenv("IMPORT_TEST_LOG", log)
	docker := filepath.Join(dir, "docker")
	script := `#!/bin/sh
printf '%s\n' "$*" >> "$IMPORT_TEST_LOG"
source=no
if [ "$1" = --context ]; then source=yes; shift 2; fi
case "$1" in
  volume|image|rm) exit 0 ;;
  run)
    if [ "$source" = yes ]; then printf invalid-archive; exit 0; fi
    cat >/dev/null; echo broken-archive >&2; exit 9 ;;
  *) exit 3 ;;
esac
`
	if e := os.WriteFile(docker, []byte(script), 0700); e != nil {
		t.Fatal(e)
	}
	c := machineTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/volumes" {
			t.Error(r.URL)
		}
		fmt.Fprint(w, `{"Volumes":[]}`)
	})
	i := importer{in: installation{home: dir, names: namesFor("")}, client: c, docker: docker, source: "orbstack", ctx: context.Background()}
	if e := i.volume("data"); e == nil || !strings.Contains(e.Error(), "broken-archive") {
		t.Fatal(e)
	}
	raw, e := os.ReadFile(log)
	if e != nil {
		t.Fatal(e)
	}
	calls := string(raw)
	if !strings.Contains(calls, "--context orbstack rm -f fabo-import-") || !strings.Contains(calls, "\nrm -f fabo-import-") || !strings.HasSuffix(calls, "volume rm data\n") {
		t.Fatal(calls)
	}
}
