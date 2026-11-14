//go:build linux || darwin

package localengine

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/OpenFabrica/FabriBay/engine/internal/guestwire"
	"github.com/OpenFabrica/FabriBay/engine/internal/hostagent"
)

func TestContainerListFiltersAndOrder(t *testing.T) {
	e := &Engine{containers: map[string]*container{
		"a": {id: "a", name: "worker", state: "running", created: 1, image: "busybox:1.37"},
		"b": {id: "b", name: "worker-old", state: "exited", created: 2, exit: 42, image: "busybox:1.37",
			imageID: "sha256:0123456789", labels: map[string]string{"archived": "yes"},
			networkName: "workspace", volumeNames: []string{"work"}},
		"c": {id: "c", name: "other", state: "running", created: 3, image: "alpine:latest"},
	}}
	for _, tc := range []struct{ filter, want string }{
		{`{}`, "c,a"},
		{`{"name":["^/worker$"]}`, "a"},
		{`{"status":"exited","name":["old$"]}`, "b"},
		{`{"exited":["42"],"ancestor":["busybox"],"network":["workspace"],"volume":["work"]}`, "b"},
		{`{"exited":{"42":true},"ancestor":["01234567"],"label!":["archived"]}`, ""},
	} {
		w := httptest.NewRecorder()
		e.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/containers/json?filters="+url.QueryEscape(tc.filter), nil))
		var rows []struct{ ID string }
		if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &rows) != nil {
			t.Fatalf("filters %s: %d %s", tc.filter, w.Code, w.Body.String())
		}
		var ids []string
		for _, row := range rows {
			ids = append(ids, row.ID)
		}
		if got := strings.Join(ids, ","); got != tc.want {
			t.Errorf("filters %s matched %q, want %q", tc.filter, got, tc.want)
		}
	}
}

func TestStartingRunningContainerReturnsNotModified(t *testing.T) {
	c := &container{id: "node", name: "fabo-k8s", state: "running"}
	e := &Engine{containers: map[string]*container{c.id: c}}
	w := httptest.NewRecorder()
	e.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/containers/fabo-k8s/start", nil))
	if w.Code != http.StatusNotModified {
		t.Fatalf("starting a running container returned %d: %s", w.Code, w.Body.String())
	}
}

func TestCreatePlatformMismatchAllowsDockerToPullAgain(t *testing.T) {
	e := &Engine{images: map[string]Image{"local": {
		ConfigBlob: []byte(`{"os":"linux","architecture":"arm64"}`),
		Cmd:        []string{"/bin/true"},
	}}}
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/containers/create?platform=linux%2Famd64", strings.NewReader(`{"Image":"local"}`))
	e.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("create with mismatched image returned %d: %s", w.Code, w.Body.String())
	}
}

func TestRenamePersistsNameAndRejectsConflicts(t *testing.T) {
	id := strings.Repeat("a", 64)
	c := &container{id: id, name: "old", state: "created", run: guestwire.RunSpec{ID: id}}
	dir := t.TempDir()
	e := &Engine{containers: map[string]*container{id: c, "other": {name: "taken"}}, state: &State{dir: dir}}
	h := e.Handler()
	rename := func(key, name string, status int) {
		t.Helper()
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1.44/containers/"+key+"/rename?name="+name, nil))
		if w.Code != status {
			t.Fatalf("rename %s to %s: got %d, want %d: %s", key, name, w.Code, status, w.Body.String())
		}
	}
	rename("old", "bad%20name", http.StatusBadRequest)
	rename("old", "taken", http.StatusConflict)
	rename("old", "old", http.StatusConflict)
	rename("old", defaultManagedBuilder, http.StatusConflict)
	rename("old", "new", http.StatusNoContent)
	rename("old", "unused", http.StatusNotFound)
	restored, err := e.state.restore()
	if err != nil || restored[id] == nil || restored[id].name != "new" {
		t.Fatalf("rename did not survive restore: %+v, %v", restored, err)
	}
	e.state.dir = filepath.Join(dir, "absent")
	rename("new", "failed", http.StatusInternalServerError)
	if c.name != "new" || e.eventBus().next != 1 {
		t.Fatalf("failed rename changed name or published an event: %s, %d", c.name, e.eventBus().next)
	}
}

func TestCreateRejectsUnsupportedHostOptions(t *testing.T) {
	e := &Engine{
		images:     map[string]Image{"local": {Cmd: []string{"/bin/test"}, Layers: []hostagent.Layer{{Digest: "base"}, {Digest: "top"}}}},
		containers: make(map[string]*container),
	}
	h := e.Handler()
	for _, tt := range []struct {
		name, body string
		status     int
	}{
		{"privileged", `{"Image":"local","HostConfig":{"NetworkMode":"none","Privileged":true}}`, 400},
		{"invalid privileged type", `{"Image":"local","HostConfig":{"Privileged":"true"}}`, 400},
		{"invalid init type", `{"Image":"local","HostConfig":{"Init":"true"}}`, 400},
		{"invalid supplementary group", `{"Image":"local","HostConfig":{"GroupAdd":["group:other"]}}`, 400},
		{"invalid ulimit order", `{"Image":"local","HostConfig":{"Ulimits":[{"Name":"nofile","Soft":512,"Hard":256}]}}`, 400},
		{"unknown ulimit", `{"Image":"local","HostConfig":{"Ulimits":[{"Name":"unknown","Soft":10,"Hard":20}]}}`, 400},
		{"host network sysctl", `{"Image":"local","HostConfig":{"NetworkMode":"host","Sysctls":{"net.core.somaxconn":"256"}}}`, 400},
		{"shared network sysctl", `{"Image":"local","HostConfig":{"NetworkMode":"container:other","Sysctls":{"net.core.somaxconn":256}}}`, 400},
		{"global sysctl", `{"Image":"local","HostConfig":{"Sysctls":{"vm.overcommit_memory":"1"}}}`, 400},
		{"invalid extra host type", `{"Image":"local","HostConfig":{"ExtraHosts":"test:127.0.0.1"}}`, 400},
		{"invalid extra host address", `{"Image":"local","HostConfig":{"ExtraHosts":["test:somewhere"]}}`, 400},
		{"invalid extra host line", `{"Image":"local","HostConfig":{"ExtraHosts":["test:127.0.0.1\n::1 other"]}}`, 400},
		{"invalid DNS server", `{"Image":"local","HostConfig":{"Dns":["not-an-ip"]}}`, 400},
		{"invalid search line", `{"Image":"local","HostConfig":{"DnsSearch":["test\nnameserver 192.0.2.53"]}}`, 400},
		{"invalid resolver option", `{"Image":"local","HostConfig":{"DnsOptions":["ndots:2 debug"]}}`, 400},
		{"shared network DNS", `{"Image":"local","HostConfig":{"NetworkMode":"container:other","Dns":["192.0.2.53"]}}`, 400},
		{"mount", `{"Image":"local","HostConfig":{"NetworkMode":"none","Binds":["/:/proc"]}}`, 400},
		{"missing network container", `{"Image":"local","HostConfig":{"NetworkMode":"container:other"}}`, 404},
		{"empty network container", `{"Image":"local","HostConfig":{"NetworkMode":"container:"}}`, 400},
		{"shared network hostname", `{"Image":"local","Hostname":"custom","HostConfig":{"NetworkMode":"container:other"}}`, 400},
		{"static IP", `{"Image":"local","NetworkingConfig":{"EndpointsConfig":{"bridge":{"IPAMConfig":{"IPv4Address":"172.17.0.40"}}}}}`, 400},
		{"unsupported protocol", `{"Image":"local","HostConfig":{"PortBindings":{"80/sctp":[{"HostPort":"8080"}]}}}`, 400},
		{"publish without bridge", `{"Image":"local","HostConfig":{"NetworkMode":"none","PortBindings":{"80/tcp":[{"HostPort":"8080"}]}}}`, 400},
		{"none", `{"Image":"local","HostConfig":{"NetworkMode":"none"}}`, 201},
	} {
		t.Run(tt.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1.44/containers/create?name=test", strings.NewReader(tt.body)))
			if w.Code != tt.status {
				t.Fatalf("status %d: %s", w.Code, w.Body.String())
			}
		})
	}
	if len(e.containers) != 1 {
		t.Fatalf("created %d containers", len(e.containers))
	}
	for _, c := range e.containers {
		if strings.Join(c.run.Layers, ",") != "top,base" {
			t.Fatalf("unexpected layer order: %v", c.run.Layers)
		}
	}
}

func TestExtraHostsSurviveStateAndEndpointChanges(t *testing.T) {
	e := &Engine{
		images:     map[string]Image{"local": {Cmd: []string{"/bin/test"}}},
		containers: map[string]*container{}, state: &State{dir: t.TempDir()},
	}
	h := e.Handler()
	request := func(method, path, body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(method, path, strings.NewReader(body)))
		return w
	}
	w := request("POST", "/containers/create?name=test",
		`{"Image":"local","Hostname":"worker","HostConfig":{"ExtraHosts":["demo:192.0.2.5","loop6=[::1]","host.docker.internal:host-gateway"]}}`)
	if w.Code != http.StatusCreated {
		t.Fatal(w.Body.String())
	}
	var err error
	e.containers, err = e.state.restore()
	if err != nil {
		t.Fatal(err)
	}
	c, err := e.find("test")
	if err != nil {
		t.Fatal(err)
	}
	endpoints := c.endpoints()
	if len(endpoints) != 1 || len(endpoints[0].Network.ExtraHosts) != 0 {
		t.Fatalf("extra hosts leaked into a bridge endpoint: %+v", endpoints)
	}
	c.setEndpoints(nil)
	c.setEndpoints(endpoints)
	w = request("GET", "/containers/test/json", "")
	var inspect struct {
		Config     struct{ Hostname string }
		HostConfig struct{ ExtraHosts []string }
	}
	if err := json.Unmarshal(w.Body.Bytes(), &inspect); err != nil {
		t.Fatal(err)
	}
	if inspect.Config.Hostname != "worker" || strings.Join(inspect.HostConfig.ExtraHosts, ",") != "demo:192.0.2.5,loop6:::1,host.docker.internal:host-gateway" {
		t.Fatalf("extra hosts or hostname lost: %s", w.Body.String())
	}
}

func TestVMPrivilegedContainerPersistsRunFlag(t *testing.T) {
	e := &Engine{
		remoteGuest: true, images: map[string]Image{"local": {Cmd: []string{"/bin/test"}}},
		containers: map[string]*container{}, state: &State{dir: t.TempDir()},
	}
	h := e.Handler()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/containers/create?name=builder",
		strings.NewReader(`{"Image":"local","HostConfig":{"NetworkMode":"none","Privileged":true}}`)))
	if w.Code != http.StatusCreated {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	restored, err := e.state.restore()
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range restored {
		if c.run.Flags&guestwire.RunPrivileged == 0 {
			t.Fatal("restored VM container lost its privileged mode")
		}
	}
	e.containers = restored
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/containers/builder/json", nil))
	var info struct{ HostConfig struct{ Privileged bool } }
	if err := json.NewDecoder(w.Body).Decode(&info); err != nil || w.Code != http.StatusOK || !info.HostConfig.Privileged {
		t.Fatalf("inspect response HTTP %d, %+v, %v", w.Code, info, err)
	}
}

// Default and explicit bridge requests must reserve different addresses,
// including when the first container is stopped and restored from disk.
func TestBridgeAddressesSurviveRestore(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	state, err := OpenState(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	e := &Engine{
		images:     map[string]Image{"local": {Cmd: []string{"/bin/test"}, Layers: []hostagent.Layer{{Digest: "layer"}}}},
		containers: make(map[string]*container),
		state:      state,
	}
	create := func(name, body string) guestwire.Network {
		t.Helper()
		w := httptest.NewRecorder()
		e.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/containers/create?name="+name, strings.NewReader(body)))
		if w.Code != http.StatusCreated {
			t.Fatalf("create %s: status %d: %s", name, w.Code, w.Body.String())
		}
		var response struct{ ID string }
		if err := json.NewDecoder(w.Body).Decode(&response); err != nil {
			t.Fatal(err)
		}
		return e.containers[response.ID].run.Network
	}
	first := create("first", `{"Image":"local"}`)
	e.containers, err = state.restore()
	if err != nil {
		t.Fatal(err)
	}
	second := create("second", `{"Image":"local","HostConfig":{"NetworkMode":"bridge"}}`)
	if first.Bridge != guestwire.DefaultBridge || second.Bridge != guestwire.DefaultBridge || first.Gateway != second.Gateway || first.IP == second.IP {
		t.Fatalf("unexpected endpoints: %+v, %+v", first, second)
	}
}

// Inspect uses the PID of the live guest run and clears it after exit.
func TestInspectReportsOnlyLiveContainerPID(t *testing.T) {
	id := strings.Repeat("b", 64)
	c := &container{id: id, name: "demo", state: "starting"}
	c.nextAttempt()
	e := &Engine{containers: map[string]*container{id: c}}
	h := e.Handler()
	check := func(want uint32) {
		t.Helper()
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/containers/demo/json", nil))
		var info struct{ State struct{ Pid *uint32 } }
		if err := json.NewDecoder(w.Body).Decode(&info); err != nil || w.Code != http.StatusOK || info.State.Pid == nil || *info.State.Pid != want {
			t.Fatalf("expected PID %d, got HTTP %d, %+v, %v", want, w.Code, info, err)
		}
	}
	e.onEvent(guestwire.RunEvent{Kind: guestwire.StartedMessage, ID: id, PID: 4242})
	check(4242)
	e.onEvent(guestwire.RunEvent{Kind: guestwire.ExitedMessage, ID: id})
	check(0)
}

// Pruning an empty engine must return an empty result without creating or
// pulling a managed builder.
func TestBuildPruneWithoutBuilder(t *testing.T) {
	e := &Engine{}
	w := httptest.NewRecorder()
	e.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1.44/build/prune?all=1", nil))
	var result buildPruneResult
	if err := json.NewDecoder(w.Body).Decode(&result); err != nil || w.Code != http.StatusOK ||
		result.CachesDeleted == nil || len(result.CachesDeleted) != 0 || result.SpaceReclaimed != 0 {
		t.Fatalf("unexpected prune response HTTP %d, %+v, %v", w.Code, result, err)
	}
}
