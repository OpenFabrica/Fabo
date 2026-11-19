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

// helperDigest is a well-formed digest for the helper image tests.
var helperDigest = "@sha256:" + strings.Repeat("ab12", 16)

// importSourceScript is a fake docker CLI for a volume import: the source
// lists an official alpine and a look-alike busybox, and each call is logged.
const importSourceScript = `#!/bin/sh
printf '%s\n' "$*" >> "$IMPORT_TEST_LOG"
source=no
if [ "$1" = --context ]; then source=yes; shift 2; fi
case "$source $1 $2" in
  "yes image ls")
    echo '{"Repository":"busybox","Tag":"latest"}'
    echo '{"Repository":"alpine","Tag":"3.22"}'
    echo '{"Repository":"nginx","Tag":"latest"}'
    exit 0 ;;
  "yes image inspect")
    echo '{"RepoTags":["busybox:latest"],"RepoDigests":[]}'
    echo '{"RepoTags":["alpine:3.22"],"RepoDigests":["alpine'"$HELPER_DIGEST"'"]}'
    exit 0 ;;
esac
`

func TestImportVolumeRollsBackFailedCopy(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "calls")
	t.Setenv("IMPORT_TEST_LOG", log)
	t.Setenv("HELPER_DIGEST", helperDigest)
	docker := filepath.Join(dir, "docker")
	script := importSourceScript + `case "$1" in
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
		switch r.URL.Path {
		case "/volumes":
			fmt.Fprint(w, `{"Volumes":[]}`)
		case "/images/json":
			fmt.Fprintf(w, `[{"RepoTags":["index.docker.io/library/busybox:latest"],"RepoDigests":["index.docker.io/library/busybox%s"]}]`, helperDigest)
		default:
			t.Error(r.URL)
		}
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
	// Each side runs its own official image by name, with sh as the
	// entrypoint and no implicit pull; nothing is carried from the source.
	if !strings.Contains(calls, "-v data:/from:ro --pull never --entrypoint sh alpine:3.22 -c tar -C /from -cf - .\n") ||
		!strings.Contains(calls, "-v data:/to --pull never --entrypoint sh index.docker.io/library/busybox:latest -c tar -C /to -xpf -\n") ||
		strings.Contains(calls, " save ") || strings.Contains(calls, "load") {
		t.Fatal(calls)
	}
}

// TestOnlyAnOfficialImageHelps: a volume copy's helper runs only an image
// the engine pulled from an official repository, as its repo digests show:
// never one that only has a name like one, wherever it came from.
func TestOnlyAnOfficialImageHelps(t *testing.T) {
	d := helperDigest
	for _, good := range []string{"busybox", "library/alpine", "docker.io/library/debian", "index.docker.io/library/busybox", "mirror.gcr.io/library/ubuntu", "public.ecr.aws/docker/library/busybox"} {
		if !officialDigest(good + d) {
			t.Errorf("%s is official", good)
		}
	}
	for _, bad := range []string{
		"ghcr.io/x/library/busybox", "localhost:5000/library/alpine", "localhost/x/library/busybox",
		"evil/alpine", "docker.io/evil/alpine", "mirror.gcr.io/evil/library/alpine",
		"public.ecr.aws/x/library/alpine", "alpinelinux", "nginx", "busybox:1", "index.docker.io/evil/busybox",
	} {
		if officialDigest(bad + d) {
			t.Errorf("%s is not official", bad)
		}
	}
	for _, bad := range []string{"busybox", "busybox:1", "busybox@sha256:abc", "busybox@sha512:" + strings.Repeat("a", 64), "busybox@sha256:" + strings.Repeat("A", 64), "busybox@sha256:" + strings.Repeat("g", 64)} {
		if officialDigest(bad) {
			t.Errorf("%s is not a digest", bad)
		}
	}
	rec := func(tags any, digests ...string) map[string]any {
		list := []any{}
		for _, d := range digests {
			list = append(list, d)
		}
		return map[string]any{"RepoTags": tags, "RepoDigests": list}
	}
	tags := func(names ...string) any {
		list := []any{}
		for _, n := range names {
			list = append(list, n)
		}
		return list
	}
	records := []map[string]any{
		// Untagged: a load or build without a name.
		rec(nil),
		rec(tags("localhost/imp-x/library/busybox:9")),
		// Re-tagged from the official one: no digest of its own.
		rec(tags("alpine:latest")),
		// Called busybox, its digest another repository's.
		rec(tags("busybox:1"), "evil/tools"+d),
		// An official digest under a name of another repository.
		rec(tags("evil/alpine:1"), "alpine"+d),
		rec(tags("ghcr.io/x/library/busybox:1"), "ghcr.io/x/library/busybox"+d),
		rec(tags("mirror.gcr.io/library/alpine:latest"), "mirror.gcr.io/library/alpine"+d),
		rec(tags("busybox:latest"), "busybox"+d),
	}
	if got := pickHelper(records); got != "mirror.gcr.io/library/alpine:latest" {
		t.Errorf("picked %q", got)
	}
	if got := pickHelper(records[:6]); got != "" {
		t.Errorf("picked a look-alike %q", got)
	}
	if got := pickHelper(records[7:]); got != "busybox:latest" {
		t.Errorf("picked %q", got)
	}
	if got := pickHelper([]map[string]any{rec(nil, "busybox"+d), rec(tags("busybox:"), "busybox"+d)}); got != "" {
		t.Errorf("picked %q without a name to run it by", got)
	}
}

// TestImportHelperIsFetchedNotBroughtOver: with no official image in Fabo,
// the helper is busybox fetched by Fabo itself, from the mirror first; a
// failed fetch is tried once per import; and Docker Hub's busybox is not
// pulled over a busybox:latest of the user's.
func TestImportHelperIsFetchedNotBroughtOver(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "calls")
	t.Setenv("IMPORT_TEST_LOG", log)
	docker := filepath.Join(dir, "docker")
	// The fake docker marks a successful pull here.
	marker := filepath.Join(dir, "pulled")
	t.Setenv("IMPORT_TEST_PULLED", marker)
	c := machineTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/images/json" {
			t.Error(r.URL)
		}
		// Only a look-alike, until the mirror's busybox is pulled.
		images := `[{"RepoTags":["alpine:latest"],"RepoDigests":[]}`
		if _, e := os.Stat(marker); e == nil {
			images += fmt.Sprintf(`,{"RepoTags":["mirror.gcr.io/library/busybox:latest"],"RepoDigests":["mirror.gcr.io/library/busybox%s"]}`, helperDigest)
		}
		fmt.Fprint(w, images+"]")
	})
	setScript := func(pullExit, inspectExit int) {
		script := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' \"$*\" >> \"$IMPORT_TEST_LOG\"\ncase \"$1\" in\n  pull) [ %[1]d = 0 ] && touch \"$IMPORT_TEST_PULLED\"; exit %[1]d ;;\n  image) exit %[2]d ;;\nesac\nexit 0\n", pullExit, inspectExit)
		if e := os.WriteFile(docker, []byte(script), 0700); e != nil {
			t.Fatal(e)
		}
		os.Remove(log)
		os.Remove(marker)
	}
	calls := func() string { raw, _ := os.ReadFile(log); return string(raw) }

	// The mirror answers.
	setScript(0, 1)
	i := importer{in: installation{home: dir, names: namesFor("")}, client: c, docker: docker, source: "orbstack", ctx: context.Background()}
	if got, e := i.helperImage(); e != nil || got != "mirror.gcr.io/library/busybox:latest" {
		t.Fatal(got, e)
	}
	if got := calls(); got != "pull -q mirror.gcr.io/library/busybox\n" {
		t.Fatalf("calls %q", got)
	}

	// Nothing answers: Docker Hub is tried only while no busybox:latest of
	// the user's would lose its name, and the import tries only once.
	setScript(1, 1)
	i = importer{in: installation{home: dir, names: namesFor("")}, client: c, docker: docker, source: "orbstack", ctx: context.Background()}
	if _, e := i.helperImage(); e == nil || !strings.Contains(e.Error(), "official image") {
		t.Fatal(e)
	}
	if got := calls(); !strings.Contains(got, "pull -q mirror.gcr.io/library/busybox\n") || !strings.Contains(got, "pull -q busybox\n") || strings.Contains(got, "--context") {
		t.Fatalf("calls %q", got)
	}
	os.Remove(log)
	if _, e := i.helperImage(); e == nil || strings.Contains(calls(), "pull") {
		t.Fatalf("pulled again in the same import: %q", calls())
	}
	setScript(1, 0)
	i = importer{in: installation{home: dir, names: namesFor("")}, client: c, docker: docker, source: "orbstack", ctx: context.Background()}
	if _, e := i.helperImage(); e == nil || strings.Contains(calls(), "pull -q busybox\n") {
		t.Fatalf("pulled busybox over the user's own: %q", calls())
	}
}

// TestImportMachineReadableModes covers the desktop's two entry points:
// --sources and --json parse, and sources skip a missing context, a context
// pointing back at this engine, and mark a silent engine as not running.
func TestImportMachineReadableModes(t *testing.T) {
	if o, e := parseImport([]string{"--sources"}); e != nil || !o.sources {
		t.Fatal(o, e)
	}
	if o, e := parseImport([]string{"desktop-linux", "--json"}); e != nil || !o.asJSON || o.source != "desktop-linux" {
		t.Fatal(o, e)
	}
	if _, e := parseImport([]string{"orbstack", "--sources"}); e == nil {
		t.Fatal("--sources after a source accepted")
	}
	dir := t.TempDir()
	t.Setenv("FABO_SOCKET", filepath.Join(dir, "fabo.sock"))
	t.Setenv("IMPORT_TEST_DIR", dir)
	docker := filepath.Join(dir, "docker")
	// orbstack answers; desktop-linux's context is missing.
	script := `#!/bin/sh
case "$2 $3 $4" in
  "orbstack context inspect") echo unix:///tmp/orbstack.sock ;;
  "orbstack version --format") echo 27.0 ;;
  *) exit 1 ;;
esac
`
	if e := os.WriteFile(docker, []byte(script), 0700); e != nil {
		t.Fatal(e)
	}
	in := installation{home: dir, names: namesFor("")}
	got := listImportSources(context.Background(), in, docker)
	if !reflect.DeepEqual(got, []importSourceInfo{{"orbstack", "OrbStack", true}}) {
		t.Fatal(got)
	}
	// A source context that is this engine's socket is not offered.
	script = "#!/bin/sh\necho unix://$IMPORT_TEST_DIR/fabo.sock\n"
	if e := os.WriteFile(docker, []byte(script), 0700); e != nil {
		t.Fatal(e)
	}
	if got := listImportSources(context.Background(), in, docker); len(got) != 0 {
		t.Fatal(got)
	}
}
