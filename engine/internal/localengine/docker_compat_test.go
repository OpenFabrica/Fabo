//go:build linux || darwin

package localengine

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// serve runs one request through the engine's handler and decodes its JSON.
func serve(t *testing.T, e *Engine, method, path, body string, out any) int {
	t.Helper()
	w := httptest.NewRecorder()
	e.Handler().ServeHTTP(w, httptest.NewRequest(method, path, strings.NewReader(body)))
	if out != nil && w.Code < 300 {
		if err := json.Unmarshal(w.Body.Bytes(), out); err != nil {
			t.Fatalf("%s %s: %v: %s", method, path, err, w.Body)
		}
	}
	return w.Code
}

// Testcontainers gates on the engine version as a Docker version (at least
// 1.6.0), so /version and /info report the Docker Engine release whose API
// Fabo serves, and Fabo's own release beside it.
func TestVersionIsADockerEngineVersionWithFaboBesideIt(t *testing.T) {
	e := &Engine{ctx: t.Context(), containers: map[string]*container{}, release: "0.6.0"}
	var version struct {
		Version, FaboVersion, ApiVersion string
		Components                       []struct {
			Name, Version string
			Details       map[string]any
		}
	}
	serve(t, e, http.MethodGet, "/v1.32/version", "", &version)
	major, err := strconv.Atoi(strings.Split(version.Version, ".")[0])
	if err != nil || major < 25 || version.Version != dockerEngineVersion || version.ApiVersion != apiVersion {
		t.Fatalf("Version %q is not a Docker Engine version for API %s", version.Version, version.ApiVersion)
	}
	if version.FaboVersion != "0.6.0" {
		t.Fatalf("FaboVersion %q", version.FaboVersion)
	}
	if len(version.Components) == 0 || version.Components[0].Name != "Engine" || version.Components[0].Version != dockerEngineVersion {
		t.Fatalf("first component: %+v", version.Components)
	}
	if !slices.ContainsFunc(version.Components, func(c struct {
		Name, Version string
		Details       map[string]any
	}) bool {
		return c.Name == "Fabo" && c.Version == "0.6.0"
	}) {
		t.Fatalf("no Fabo component: %+v", version.Components)
	}
	// The docker CLI reads every component's Details as strings.
	for _, c := range version.Components {
		if c.Details == nil {
			t.Fatalf("component %s has no Details object", c.Name)
		}
		for k, v := range c.Details {
			if _, ok := v.(string); !ok {
				t.Fatalf("component %s detail %s is %T", c.Name, k, v)
			}
		}
	}

	var info struct{ ServerVersion, FaboVersion, SystemTime string }
	serve(t, e, http.MethodGet, "/info", "", &info)
	if info.ServerVersion != version.Version || info.FaboVersion != "0.6.0" {
		t.Fatalf("/info versions %q %q", info.ServerVersion, info.FaboVersion)
	}
	// Compose 5.7 needs the daemon's time before it attaches.
	at, err := time.Parse(time.RFC3339Nano, info.SystemTime)
	if err != nil || !strings.HasSuffix(info.SystemTime, "Z") || time.Since(at).Abs() > time.Minute {
		t.Fatalf("SystemTime %q: %v", info.SystemTime, err)
	}

	// A build without an injected release still names itself.
	e.release = ""
	serve(t, e, http.MethodGet, "/version", "", &version)
	if version.FaboVersion != "fabo-go" || version.Version != dockerEngineVersion {
		t.Fatalf("unreleased build: %q %q", version.Version, version.FaboVersion)
	}
}

// Docker's merge of a create's Entrypoint and Cmd with the image's.
func TestMergeCommandFollowsDocker(t *testing.T) {
	ie, ic := []string{"/entry"}, []string{"/bin/ryuk"}
	for _, tt := range []struct {
		name                    string
		entrypoint, cmd         []string
		wantEntrypoint, wantCmd []string
	}{
		// Testcontainers (Java, .NET): "Cmd": [] means the image's Cmd.
		{"empty Cmd", nil, []string{}, ie, ic},
		{"nothing given", nil, nil, ie, ic},
		{"Cmd replaces the image's", nil, []string{"x"}, ie, []string{"x"}},
		// A non-empty Entrypoint drops the image's Cmd.
		{"Entrypoint drops image Cmd", []string{"sh"}, nil, []string{"sh"}, nil},
		{"Entrypoint and empty Cmd", []string{"sh"}, []string{}, []string{"sh"}, nil},
		{"Entrypoint and Cmd", []string{"sh"}, []string{"-c", "x"}, []string{"sh"}, []string{"-c", "x"}},
		// "Entrypoint": [] clears the image's and keeps its Cmd.
		{"empty Entrypoint", []string{}, nil, nil, ic},
	} {
		entrypoint, cmd := mergeCommand(tt.entrypoint, tt.cmd, ie, ic)
		if !slices.Equal(entrypoint, tt.wantEntrypoint) || !slices.Equal(cmd, tt.wantCmd) {
			t.Errorf("%s: got %q %q, want %q %q", tt.name, entrypoint, cmd, tt.wantEntrypoint, tt.wantCmd)
		}
	}
}

// Inspect splits the argv back into Entrypoint and Cmd, and never slices
// past a record whose stored entrypoint does not lead its argv.
func TestInspectCommandSplitsArgv(t *testing.T) {
	for _, tt := range []struct {
		entrypoint, argv, wantEntrypoint, wantCmd []string
	}{
		{nil, []string{"/bin/sh"}, nil, []string{"/bin/sh"}},
		{[]string{"/entry"}, []string{"/entry", "run"}, []string{"/entry"}, []string{"run"}},
		{[]string{"/entry"}, []string{"/entry"}, []string{"/entry"}, []string{}},
		{[]string{"/a", "/b"}, []string{"/a"}, nil, []string{"/a"}},
		{[]string{"/other"}, []string{"/entry", "run"}, nil, []string{"/entry", "run"}},
	} {
		entrypoint, cmd := inspectCommand(tt.entrypoint, tt.argv)
		if !slices.Equal(entrypoint, tt.wantEntrypoint) || cmd == nil || !slices.Equal(cmd, tt.wantCmd) {
			t.Errorf("%q in %q: got %q %q", tt.entrypoint, tt.argv, entrypoint, cmd)
		}
	}
}

// Testcontainers creates Ryuk and redis with "Cmd": []: the container runs
// the image's CMD, and inspect reports it in Config.Cmd.
func TestCreateWithEmptyCmdRunsTheImageCmd(t *testing.T) {
	e := &Engine{
		images: map[string]Image{
			"alpine": {Cmd: []string{"/bin/sh"}},
			"entry":  {Entrypoint: []string{"/entry"}, Cmd: []string{"--serve"}},
		},
		containers: map[string]*container{}, state: &State{dir: t.TempDir()},
	}
	for _, tt := range []struct {
		name, body, entrypoint, cmd string
	}{
		{"tc-cmd", `{"Image":"alpine","Cmd":[]}`, "null", `["/bin/sh"]`},
		{"tc-entry", `{"Image":"entry","Cmd":[]}`, `["/entry"]`, `["--serve"]`},
		{"tc-override", `{"Image":"entry","Entrypoint":["sh"],"Cmd":null}`, `["sh"]`, `[]`},
	} {
		if code := serve(t, e, http.MethodPost, "/v1.32/containers/create?name="+tt.name, tt.body, nil); code != http.StatusCreated {
			t.Fatalf("%s: create %d", tt.name, code)
		}
		var inspect struct {
			Path   string
			Config struct{ Entrypoint, Cmd json.RawMessage }
		}
		serve(t, e, http.MethodGet, "/containers/"+tt.name+"/json", "", &inspect)
		if string(inspect.Config.Entrypoint) != tt.entrypoint || string(inspect.Config.Cmd) != tt.cmd {
			t.Errorf("%s: Entrypoint %s Cmd %s, want %s %s", tt.name, inspect.Config.Entrypoint, inspect.Config.Cmd, tt.entrypoint, tt.cmd)
		}
	}
}

// Attach and exec start answer 101 only to a client that asked to upgrade,
// checking the header's presence, not its value; others get 200 with the
// raw stream as the body.
func TestStreamHeadUpgradesOnlyWhenAsked(t *testing.T) {
	head := func(headers string) *http.Response {
		t.Helper()
		req, err := http.ReadRequest(bufio.NewReader(strings.NewReader("POST /exec/x/start HTTP/1.1\r\nHost: fabo\r\n" + headers + "\r\n")))
		if err != nil {
			t.Fatal(err)
		}
		res, err := http.ReadResponse(bufio.NewReader(strings.NewReader(streamHead(req))), nil)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	for _, headers := range []string{"Connection: Upgrade\r\nUpgrade: tcp\r\n", "upgrade: h2c\r\n"} {
		if res := head(headers); res.StatusCode != http.StatusSwitchingProtocols || res.Header.Get("Upgrade") != "tcp" {
			t.Fatalf("%q: %s %v", headers, res.Status, res.Header)
		}
	}
	res := head("")
	if res.StatusCode != http.StatusOK || res.Header.Get("Content-Type") != "application/vnd.docker.raw-stream" || res.Header.Get("Upgrade") != "" {
		t.Fatalf("plain: %s %v", res.Status, res.Header)
	}
}

// A plain attach (docker-java) gets 200 and the stream on the same
// connection, ended by the server closing it.
func TestAttachWithoutUpgradeStreamsTheBody(t *testing.T) {
	c := &container{id: "id", name: "done", state: "exited", logs: []record{{stream: 1, data: []byte("hi\n")}}}
	e := &Engine{containers: map[string]*container{c.id: c}}
	server := httptest.NewServer(e.Handler())
	defer server.Close()
	conn, err := net.Dial("tcp", server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := fmt.Fprint(conn, "POST /containers/done/attach?logs=1&stdout=1&stream=1 HTTP/1.1\r\nHost: fabo\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	res, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	body := new(strings.Builder)
	_, err = bufio.NewReader(res.Body).WriteTo(body)
	if res.StatusCode != http.StatusOK || err != nil || body.String() != "\x01\x00\x00\x00\x00\x00\x00\x03hi\n" {
		t.Fatalf("attach: %s %q %v", res.Status, body, err)
	}
}

// Docker matches each name filter value exactly, then as an unanchored
// regular expression, against the name with and without its leading "/". A
// value that is not a valid expression only matches exactly. k3d looks up
// its nodes with `^[/]?<node>$`.
func TestNameFilterIsARegexLikeDocker(t *testing.T) {
	matches := func(pattern, name string) bool { return newNameFilter([]string{pattern}).match(name) }
	for _, p := range []string{"^/probe-tools$", "probe-tools$", "^/?probe-tools$", "^[/]?probe-tools$", "^probe-.+$", "tools|nope"} {
		if !matches(p, "probe-tools") || !matches(p, "/probe-tools") {
			t.Errorf("%q does not match probe-tools", p)
		}
	}
	for _, tt := range []struct {
		pattern, name string
		want          bool
	}{
		{"^[/]?probe-tools$", "probe-tools-2", false},
		{"^[/]?k3d-trial-tools$", "k3d-other-tools", false},
		{"^web-[0-9]+$", "web-12", true},
		{"^web-[0-9]+$", "web-x", false},
		// A dot is any character, as in Docker.
		{"^a.c$", "abc", true},
		// Not a valid expression: only an exact match.
		{"a[", "a[", true},
		{"a[", "a[b", false},
		{"^kind$", "kind", true},
		{"^kind$", "kind-control-plane", false},
		{"kind", "kind-control-plane", true},
	} {
		if got := matches(tt.pattern, tt.name); got != tt.want {
			t.Errorf("%q against %q: %v", tt.pattern, tt.name, got)
		}
	}
	// Values of one key OR together; none matches everything.
	f := newNameFilter([]string{"^[/]?one$", "^two$"})
	if !f.match("one") || !f.match("/two") || f.match("three") || !newNameFilter(nil).match("anything") {
		t.Fatal("name values do not OR together")
	}
}

// Containers, networks and volumes all match `name` as Docker does, together
// with the other filters.
func TestListsMatchNamesAsRegularExpressions(t *testing.T) {
	e := &Engine{
		state: &State{dir: t.TempDir()},
		containers: map[string]*container{
			"a": {id: "a", name: "k3d-trial-server-0", state: "running", created: 1, labels: map[string]string{"app": "k3d"}},
			"b": {id: "b", name: "k3d-trial-tools", state: "running", created: 2, labels: map[string]string{"app": "k3d"}},
			"c": {id: "c", name: "k3d-trial-server-0-copy", state: "running", created: 3},
		},
		networks: map[string]dockerNetwork{
			"k3d-trial":   {Name: "k3d-trial", ID: "n1"},
			"k3d-trial-2": {Name: "k3d-trial-2", ID: "n2"},
		},
	}
	ids := func(path, filters string) string {
		t.Helper()
		var rows []struct{ Id string }
		if code := serve(t, e, http.MethodGet, path+"?filters="+url.QueryEscape(filters), "", &rows); code != http.StatusOK {
			t.Fatalf("%s %s: %d", path, filters, code)
		}
		var out []string
		for _, row := range rows {
			out = append(out, row.Id)
		}
		return strings.Join(out, ",")
	}
	if got := ids("/containers/json", `{"name":["^[/]?k3d-trial-server-0$"],"label":["app=k3d"]}`); got != "a" {
		t.Errorf("k3d node lookup matched %q", got)
	}
	if got := ids("/containers/json", `{"name":["^k3d-trial-(server|tools)"]}`); got != "c,b,a" {
		t.Errorf("alternation matched %q", got)
	}
	if got := ids("/networks", `{"name":["^k3d-trial$"]}`); got != "n1" {
		t.Errorf("network lookup matched %q", got)
	}
	if got := ids("/networks", `{"name":["k3d-trial"]}`); got != "n1,n2" {
		t.Errorf("unanchored network lookup matched %q", got)
	}
}

// The volume list understands Docker's name, label, label!, dangling and
// driver filters.
func TestVolumeListFilters(t *testing.T) {
	e := &Engine{state: &State{dir: t.TempDir()}, containers: map[string]*container{}}
	for _, v := range []struct {
		name   string
		labels map[string]string
	}{
		{"k3d-trial-images", map[string]string{"app": "k3d", "role": "images"}},
		{"k3d-trial-images-2", map[string]string{"app": "k3d"}},
		{"data", nil},
	} {
		if _, err := e.ensureVolume(v.name, v.labels, nil); err != nil {
			t.Fatal(err)
		}
	}
	e.containers["user"] = &container{id: "user", state: "exited", volumeNames: []string{"data"}}
	names := func(filters string) string {
		t.Helper()
		var list struct {
			Volumes []struct{ Name string }
		}
		if code := serve(t, e, http.MethodGet, "/volumes?filters="+url.QueryEscape(filters), "", &list); code != http.StatusOK {
			t.Fatalf("filters %s: %d", filters, code)
		}
		var out []string
		for _, v := range list.Volumes {
			out = append(out, v.Name)
		}
		return strings.Join(out, ",")
	}
	for _, tt := range []struct{ filters, want string }{
		{``, "data,k3d-trial-images,k3d-trial-images-2"},
		{`{"name":["^[/]?k3d-trial-images$"]}`, "k3d-trial-images"},
		{`{"name":["images"]}`, "k3d-trial-images,k3d-trial-images-2"},
		{`{"label":["app=k3d","role"]}`, "k3d-trial-images"},
		{`{"label!":["app"]}`, "data"},
		{`{"dangling":["true"]}`, "k3d-trial-images,k3d-trial-images-2"},
		{`{"dangling":["0"]}`, "data"},
		{`{"driver":["local"],"name":["^data$"]}`, "data"},
		{`{"driver":["other"]}`, ""},
	} {
		if got := names(tt.filters); got != tt.want {
			t.Errorf("filters %s: %q, want %q", tt.filters, got, tt.want)
		}
	}
	for _, bad := range []string{`{"dangling":["maybe"]}`, `{"until":["1h"]}`} {
		if code := serve(t, e, http.MethodGet, "/volumes?filters="+url.QueryEscape(bad), "", nil); code != http.StatusBadRequest {
			t.Errorf("filters %s: %d", bad, code)
		}
	}
}

// Images list newest first, as Docker lists them, and by ID within a second.
func TestImagesListNewestFirst(t *testing.T) {
	e := &Engine{images: map[string]Image{
		"old:1":    {ID: "sha256:aaa", Created: 100},
		"new:1":    {ID: "sha256:ccc", Created: 300},
		"mid:1":    {ID: "sha256:bbb", Created: 200},
		"twin:1":   {ID: "sha256:ddd", Created: 200},
		"alpine:1": {ID: "sha256:000", Created: 50},
	}}
	var rows []struct {
		Id      string
		Created int64
	}
	serve(t, e, http.MethodGet, "/images/json", "", &rows)
	var got []string
	for _, row := range rows {
		got = append(got, row.Id)
	}
	if want := "sha256:ccc,sha256:bbb,sha256:ddd,sha256:aaa,sha256:000"; strings.Join(got, ",") != want {
		t.Fatalf("order %v, want %s", got, want)
	}
}
