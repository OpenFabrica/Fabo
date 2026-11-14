package main

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"image/png"
	"net/http"

	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/egoist/mygo/ui"
)

type memoryRegistry struct {
	items   map[string]registryLogin
	failure error
}

func (m *memoryRegistry) get(key string) (registryLogin, error) {
	if m.failure != nil {
		return registryLogin{}, m.failure
	}
	v, ok := m.items[key]
	if !ok {
		return v, errRegistryNotFound
	}
	return v, nil
}
func (m *memoryRegistry) set(key string, v registryLogin) error {
	if m.failure != nil {
		return m.failure
	}
	m.items[key] = v
	return nil
}
func (m *memoryRegistry) remove(key string) error {
	if m.failure != nil {
		return m.failure
	}
	delete(m.items, key)
	return nil
}
func (m *memoryRegistry) list() (map[string]string, error) {
	result := map[string]string{}
	for k, v := range m.items {
		result[k] = v.Username
	}
	return result, m.failure
}

func intercept(f *fakeEngine, handler func(http.ResponseWriter, *http.Request) bool) {
	base := f.server.Config.Handler
	f.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !handler(w, r) {
			base.ServeHTTP(w, r)
		}
	})
}
func click(t *testing.T, tt *ui.Tester, label string) {
	t.Helper()
	if err := tt.Click(label); err != nil {
		t.Fatal(err)
	}
}

func TestContainerEnvironmentAndPortsAreEditedIndependently(t *testing.T) {
	f, s := newFakeEngine(t)
	var mu sync.Mutex
	var requests []string
	var bodies []string
	intercept(f, func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method == http.MethodGet && r.URL.Path == "/containers/c-api/json" {
			fmt.Fprint(w, `{"Config":{"Env":["NODE_ENV=development","TOKEN=a=b"]},"HostConfig":{"PortBindings":{"80/tcp":[{"HostIP":"127.0.0.1","HostPort":""}],"53/udp":[{"HostIP":"::1","HostPort":"5353"}]}}}`)
			return true
		}
		if strings.HasPrefix(r.URL.Path, "/kiri/containers/") {
			var raw json.RawMessage
			_ = json.NewDecoder(r.Body).Decode(&raw)
			mu.Lock()
			requests = append(requests, r.URL.Path)
			bodies = append(bodies, string(raw))
			mu.Unlock()
			w.WriteHeader(204)
			return true
		}
		return false
	})
	s.selectRow("c-api")
	s.tab[containersPage] = 4
	s.dockMax = true
	tt := ui.NewTester(s.view, 1180, 760)
	tt.Frame()
	if !s.containerDraft.loaded || len(s.containerDraft.env) != 2 || s.containerDraft.env[1].value != "a=b" {
		t.Fatalf("draft: %+v", s.containerDraft)
	}
	// Configured dynamic port and IPv6/UDP survive unchanged.
	bindings, err := s.containerDraft.bindings()
	if err != nil || len(bindings) != 2 || bindings[1].HostPort != 0 || bindings[0].HostIP != "::1" || bindings[0].Protocol != "udp" {
		t.Fatalf("bindings: %+v %v", bindings, err)
	}
	s.containerDraft.env[0].value = "production"
	tt.Frame()
	click(t, tt, "Save environment")
	click(t, tt, "Cancel")
	if len(requests) != 0 {
		t.Fatal("cancel changed environment")
	}
	click(t, tt, "Save environment")
	click(t, tt, "Save")
	if len(requests) != 1 || requests[0] != "/kiri/containers/c-api/env" || !strings.Contains(bodies[0], "NODE_ENV=production") || !strings.Contains(bodies[0], "TOKEN=a=b") {
		t.Fatalf("requests %v %v", requests, bodies)
	}
	click(t, tt, "Save ports")
	if len(requests) != 2 || requests[1] != "/kiri/containers/c-api/ports" || !strings.Contains(bodies[1], `"host_port":0`) {
		t.Fatalf("ports: %v %v", requests, bodies)
	}
	// An empty list is an explicit removal, never JSON null.
	s.containerDraft.env = nil
	s.containerDraft.ports = nil
	s.saveContainerSettings("env")
	s.containerDraft.ports = nil
	s.saveContainerSettings("ports")
	if bodies[2] != `{"env":[]}` || bodies[3] != `{"ports":[]}` {
		t.Fatalf("empty: %v", bodies)
	}
}

func TestContainerSettingsFailureKeepsDraftAndCannotSaveUnreadSettings(t *testing.T) {
	f, s := newFakeEngine(t)
	failRead := true
	intercept(f, func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path == "/containers/c-api/json" && failRead {
			w.WriteHeader(500)
			fmt.Fprint(w, `{"message":"inspect failed"}`)
			return true
		}
		if r.URL.Path == "/kiri/containers/c-api/env" {
			w.WriteHeader(409)
			fmt.Fprint(w, `{"message":"environment saved but restart failed"}`)
			return true
		}
		return false
	})
	s.selectRow("c-api")
	s.tab[containersPage] = 4
	s.dockMax = true
	tt := ui.NewTester(s.view, 1180, 760)
	tt.Frame()
	if !tt.HasText("inspect failed") {
		t.Fatalf("read error hidden: %v", tt.Texts())
	}
	if _, ok := tt.Find("Save environment"); ok {
		t.Fatal("unread environment could be cleared")
	}
	failRead = false
	click(t, tt, "Reload")
	tt.Frame()
	s.containerDraft.env = []envEntry{{"KEEP", "draft"}}
	s.saveContainerSettings("env")
	tt.Frame()
	if !strings.Contains(s.containerDraft.problem, "restart failed") || s.containerDraft.env[0].value != "draft" {
		t.Fatalf("draft lost: %+v", s.containerDraft)
	}
}

func TestContainerDraftValidation(t *testing.T) {
	for _, entry := range []envEntry{{"", "x"}, {"A B", "x"}, {"A=B", "x"}, {"A", "x\x00"}} {
		if _, err := (containerSettings{env: []envEntry{entry}}).environment(); err == nil {
			t.Fatalf("accepted %+v", entry)
		}
	}
	for _, entry := range []portEntry{{"", "80", "0", "tcp"}, {"", "65536", "80", "tcp"}, {"example.com", "80", "80", "tcp"}, {"", "80", "80", "sctp"}} {
		if _, err := (containerSettings{ports: []portEntry{entry}}).bindings(); err == nil {
			t.Fatalf("accepted %+v", entry)
		}
	}
}

const kubeFixture = `{"items":[{"kind":"Pod","metadata":{"namespace":"default","name":"web-1","uid":"uid-1"},"spec":{"nodeName":"kiri","containers":[{"name":"web","image":"nginx"},{"name":"side","image":"busybox"}]},"status":{"phase":"Running","podIP":"10.42.0.9","containerStatuses":[{"name":"web","ready":true,"restartCount":1,"state":{"running":{}}},{"name":"side","ready":false,"restartCount":3,"state":{"waiting":{"reason":"CrashLoopBackOff"}}}]}},{"kind":"Service","metadata":{"namespace":"default","name":"web"},"spec":{"type":"LoadBalancer","clusterIP":"10.43.0.5","ports":[{"port":8080,"targetPort":80,"protocol":"TCP"}]},"status":{"loadBalancer":{"ingress":[{"ip":"127.0.0.1"}]}}}]}`

type kubeEngine struct {
	mu         sync.Mutex
	state      string
	commands   [][]string
	background []bool
	out        map[string]string
	last       []string
	exit       int
}

func installKube(t *testing.T, f *fakeEngine) *kubeEngine {
	t.Helper()
	k := &kubeEngine{state: "running", out: map[string]string{}}
	intercept(f, func(w http.ResponseWriter, r *http.Request) bool {
		k.mu.Lock()
		defer k.mu.Unlock()
		if r.URL.Path == "/containers/kiri-k8s/json" {
			if k.state == "absent" {
				w.WriteHeader(404)
				fmt.Fprint(w, `{"message":"No such container"}`)
				return true
			}
			fmt.Fprintf(w, `{"State":{"Status":%q,"Running":%v},"NetworkSettings":{"Ports":{"6443/tcp":[{"HostPort":"7443"}]}}}`, k.state, k.state == "running")
			return true
		}
		if r.URL.Path == "/containers/kiri-k8s/exec" {
			var spec struct{ Cmd []string }
			if err := json.NewDecoder(r.Body).Decode(&spec); err != nil {
				t.Error(err)
			}
			k.commands = append(k.commands, spec.Cmd)
			k.background = append(k.background, r.Header.Get("X-Kiri-Background") == "1")
			k.last = spec.Cmd
			id := fmt.Sprintf("kube-%d", len(k.commands))
			out := kubeFixture
			if len(spec.Cmd) > 5 && spec.Cmd[5] == "logs" {
				out = "pod log\n"
			}
			if len(spec.Cmd) > 5 && spec.Cmd[5] == "delete" {
				out = "pod deleted\n"
			}
			if k.exit != 0 {
				out = "kubectl failed\n"
			}
			k.out[id] = out
			fmt.Fprintf(w, `{"Id":%q}`, id)
			return true
		}
		if strings.HasPrefix(r.URL.Path, "/exec/kube-") {
			if r.Header.Get("X-Kiri-Background") != "1" && (len(k.last) < 6 || k.last[5] != "delete") {
				t.Error("passive exec start/inspect lacks background marker")
			}
			id := strings.Split(r.URL.Path, "/")[2]
			if strings.HasSuffix(r.URL.Path, "/json") {
				fmt.Fprintf(w, `{"Running":false,"ExitCode":%d}`, k.exit)
				return true
			}
			conn, buf, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return true
			}
			defer conn.Close()
			out := k.out[id]
			head := make([]byte, 8)
			head[0] = 1
			binary.BigEndian.PutUint32(head[4:], uint32(len(out)))
			buf.WriteString("HTTP/1.1 101 UPGRADED\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n")
			buf.Write(head)
			buf.WriteString(out)
			buf.Flush()
			return true
		}
		return false
	})
	return k
}
func TestKubernetesBrowsingAndPodLogs(t *testing.T) {
	f, s := newFakeEngine(t)
	k := installKube(t, f)
	s.navigate(kubernetesPage)
	tt := ui.NewTester(s.view, 1180, 760)
	tt.Frame()
	if m := has(tt, "web-1", "CrashLoopBackOff", "1/2"); m != "" {
		t.Fatalf("missing %s: %v", m, tt.Texts())
	}
	click(t, tt, "Details for web-1")
	click(t, tt, "Logs")
	tt.Frame()
	if !tt.HasText("pod log") {
		t.Fatalf("logs: %v", tt.Texts())
	}
	click(t, tt, "Previous run")
	tt.Frame()
	k.mu.Lock()
	last := append([]string{}, k.last...)
	background := append([]bool{}, k.background...)
	k.mu.Unlock()
	if !strings.Contains(strings.Join(last, " "), "--previous") {
		t.Fatalf("previous: %v", last)
	}
	for _, b := range background {
		if !b {
			t.Fatal("browsing keeps VM awake")
		}
	}
	s.selectRow("")
	click(t, tt, "Services")
	if !tt.HasText("10.43.0.5") {
		t.Fatalf("services: %v", tt.Texts())
	}
	click(t, tt, "Details for web")
	tt.Frame()
	if !tt.HasText("8080 → 80/TCP") {
		t.Fatalf("service ports: %v", tt.Texts())
	}
}
func TestKubernetesStoppedAndFailedExec(t *testing.T) {
	for _, state := range []string{"absent", "exited"} {
		t.Run(state, func(t *testing.T) {
			f, s := newFakeEngine(t)
			k := installKube(t, f)
			k.mu.Lock()
			k.state = state
			k.mu.Unlock()
			s.navigate(kubernetesPage)
			if s.kube.state != state || len(k.commands) != 0 {
				t.Fatalf("browsing woke cluster: %+v", s.kube)
			}
		})
	}
	f, s := newFakeEngine(t)
	k := installKube(t, f)
	k.mu.Lock()
	k.exit = 1
	k.mu.Unlock()
	s.navigate(kubernetesPage)
	if !strings.Contains(s.kube.problem, "code 1") || s.kube.retryAt.IsZero() {
		t.Fatalf("failed kubectl: %+v", s.kube)
	}
	count := len(k.commands)
	s.refreshKubernetes()
	if len(k.commands) != count {
		t.Fatal("failure retries without cooldown")
	}
}
func TestKubernetesActionsUseLauncherAndConfirmDataLoss(t *testing.T) {
	_, s := newFakeEngine(t)
	var calls [][]string
	s.launcher = func(_ context.Context, args ...string) ([]byte, error) {
		if len(args) > 0 && args[0] == "k8s" {
			calls = append(calls, append([]string{}, args...))
		}
		return nil, nil
	}
	s.navigate(settingsPage)
	s.settingsTab = indexSettings("kubernetes")
	tt := ui.NewTester(s.view, 1180, 760)
	click(t, tt, "Start")
	s.kube.lan = true
	s.kube.apiPort = "7443"
	s.kube.sans = "192.168.1.9"
	tt.Frame()
	click(t, tt, "Save network settings")
	click(t, tt, "Save")
	click(t, tt, "Delete cluster…")
	click(t, tt, "Cancel")
	for _, v := range calls {
		if reflect.DeepEqual(v, []string{"k8s", "delete"}) {
			t.Fatal("cancel deleted cluster")
		}
	}
	if !reflect.DeepEqual(calls[0], []string{"k8s", "start"}) || !reflect.DeepEqual(calls[1], []string{"k8s", "set", "--lan", "on", "--tls-san", "192.168.1.9", "--port", "7443"}) {
		t.Fatalf("launcher: %v", calls)
	}
}
func indexSettings(id string) int {
	for i, t := range settingsTabs {
		if t.id == id {
			return i
		}
	}
	return -1
}

func TestRegistryLoginFailureAndAuthenticatedPull(t *testing.T) {
	f, s := newFakeEngine(t)
	fail := true
	var sentAuth string
	intercept(f, func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path == "/auth" {
			if fail {
				w.WriteHeader(401)
				fmt.Fprint(w, `{"message":"credentials rejected"}`)
			} else {
				fmt.Fprint(w, `{"IdentityToken":"registry-token"}`)
			}
			return true
		}
		if r.URL.Path == "/images/create" {
			sentAuth = r.Header.Get("X-Registry-Auth")
			if r.URL.Query().Get("fromImage") != "ghcr.io/team/app@sha256:1234" {
				t.Errorf("digest: %s", r.URL)
			}
			fmt.Fprint(w, "{\"status\":\"complete\"}\n")
			return true
		}
		return false
	})
	s.navigate(settingsPage)
	s.settingsTab = indexSettings("registries")
	tt := ui.NewTester(s.view, 1180, 760)
	s.registries.server = "ghcr.io"
	s.registries.username = "user"
	s.registries.secret = "bad-secret"
	tt.Frame()
	click(t, tt, "Sign in")
	if !tt.HasText("credentials rejected") || s.registries.secret != "" {
		t.Fatalf("failed login: %+v", s.registries)
	}
	if _, err := os.Stat(s.credentials.path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed auth wrote config: %v", err)
	}
	fail = false
	s.registries.secret = "good-secret"
	tt.Frame()
	click(t, tt, "Sign in")
	login, found, err := s.credentials.find(t.Context(), "ghcr.io")
	if err != nil || !found || login.Username != tokenUsername || login.Secret != "registry-token" {
		t.Fatalf("saved auth: %+v %v", login, err)
	}
	raw, _ := os.ReadFile(s.credentials.path)
	if strings.Contains(string(raw), "secret") || strings.Contains(string(raw), "registry-token") {
		t.Fatal("secret leaked into Docker config")
	}
	s.form = form{kind: "pull", image: "ghcr.io/team/app@sha256:1234"}
	if !s.submit() {
		t.Fatal(s.form.problem)
	}
	s.form = form{}
	auth, err := base64.URLEncoding.DecodeString(sentAuth)
	if err != nil || !strings.Contains(string(auth), `"identitytoken":"registry-token"`) {
		t.Fatalf("missing pull auth: %v", err)
	}
	s.navigate(settingsPage)
	s.settingsTab = indexSettings("registries")
	tt.Frame()
	click(t, tt, "Sign out…")
	click(t, tt, "Cancel")
	if _, found, _ := s.credentials.find(t.Context(), "ghcr.io"); !found {
		t.Fatal("cancel erased auth")
	}
	click(t, tt, "Sign out…")
	click(t, tt, "Sign out")
	if _, found, _ := s.credentials.find(t.Context(), "ghcr.io"); found {
		t.Fatal("signout retained auth")
	}
}

func TestRegistryHelpersAndConfigPreservation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	original := `{"credsStore":"test","credHelpers":{"ghcr.io":"own"},"currentContext":"work","large":9007199254740993,"auths":{"docker.io":{"auth":"dXNlcjpwYXNz"}}}`
	if err := os.WriteFile(path, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	var actions []string
	store := &registryStore{path: path, native: &memoryRegistry{items: map[string]registryLogin{}}, helper: func(_ context.Context, name, action string, input []byte) ([]byte, error) {
		actions = append(actions, name+"/"+action)
		switch action {
		case "get":
			return []byte(`{"Username":"private-user","Secret":"password"}`), nil
		case "list":
			return []byte(`{"ghcr.io":"private-user"}`), nil
		}
		return nil, nil
	}}
	if err := store.save(t.Context(), "ghcr.io", registryLogin{"u", "p"}); err != nil {
		t.Fatal(err)
	}
	if login, found, err := store.find(t.Context(), "ghcr.io"); err != nil || !found || login.Username != "private-user" {
		t.Fatalf("helper not used: %+v %v", login, err)
	}
	accounts, err := store.accounts(t.Context())
	if err != nil || len(accounts) != 2 {
		t.Fatalf("accounts: %v %v", accounts, err)
	}
	if err := store.logout(t.Context(), "ghcr.io"); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	if !strings.Contains(string(raw), "9007199254740993") || !strings.Contains(string(raw), `"currentContext": "work"`) || !strings.Contains(string(raw), "dXNlcjpwYXNz") {
		t.Fatalf("unrelated config lost: %s", raw)
	}
	if strings.Join(actions[:2], ",") != "own/store,own/get" {
		t.Fatalf("helper order: %v", actions)
	}
	os.WriteFile(path, []byte("broken"), 0600)
	before := len(actions)
	if err := store.save(t.Context(), "ghcr.io", registryLogin{"u", "p"}); err == nil || len(actions) != before {
		t.Fatal("corrupt config overwritten or credentials changed")
	}
}
func TestRegistryKeys(t *testing.T) {
	for _, name := range []string{"", "docker.io", "registry-1.docker.io", dockerHub} {
		if key, err := registryKey(name); err != nil || key != dockerHub {
			t.Fatalf("hub %s: %s %v", name, key, err)
		}
	}
	for _, name := range []string{"https://user:secret@ghcr.io", "ghcr.io/path", "--help", "https://ghcr.io?q=secret"} {
		if _, err := registryKey(name); err == nil {
			t.Fatalf("accepted %s", name)
		}
	}
	if key, _ := registryForImage("localhost:5000/team/app:tag"); key != "localhost:5000" {
		t.Fatal(key)
	}
	if key, _ := registryForImage("team/app:tag"); key != dockerHub {
		t.Fatal(key)
	}
}

func TestAdvancedScreenshots(t *testing.T) {
	dir := os.Getenv("KIRI_SHOTS")
	if dir == "" {
		t.Skip("set KIRI_SHOTS")
	}
	for _, theme := range []string{"dark", "light"} {
		f, s := newFakeEngine(t)
		installKube(t, f)
		s.prefs.Theme = theme
		s.prefs.Lang = "zh"
		s.prefsFile = filepath.Join(t.TempDir(), "desktop.json")
		shot := func(name string) {
			t.Helper()
			tt := ui.NewTester(s.view, 1180, 760)
			tt.Frame()
			file, err := os.Create(filepath.Join(dir, name+"-"+theme+".png"))
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			if err := png.Encode(file, tt.Image()); err != nil {
				t.Fatal(err)
			}
		}
		s.selectRow("c-api")
		s.tab[containersPage] = 4
		s.dockMax = true
		shot("container-settings")
		s.navigate(kubernetesPage)
		shot("kubernetes")
		s.selectRow("Pod/default/web-1")
		s.tab[kubernetesPage] = 1
		shot("pod-logs")
		s.navigate(settingsPage)
		s.settingsTab = indexSettings("registries")
		shot("registries")
		s.settingsTab = indexSettings("kubernetes")
		shot("kubernetes-settings")
	}
}

func TestContainerSettingRowsCanBeRemovedWithoutLosingOtherRows(t *testing.T) {
	_, s := newFakeEngine(t)
	s.selectRow("c-api")
	s.tab[containersPage] = 4
	s.dockMax = true
	s.containerDraft = containerSettings{id: "c-api", loaded: true, env: []envEntry{{"A", "one"}, {"B", "two"}, {"C", "three"}}, ports: []portEntry{{host: "1", port: "80", protocol: "tcp"}, {host: "2", port: "80", protocol: "tcp"}, {host: "3", port: "80", protocol: "tcp"}}}
	tt := ui.NewTester(s.view, 1180, 760)
	click(t, tt, "Remove variable 1")
	if len(s.containerDraft.env) != 2 || s.containerDraft.env[0].name != "B" || s.containerDraft.env[1].value != "three" {
		t.Fatal("remaining environment rows changed")
	}
	click(t, tt, "Remove port 2")
	if len(s.containerDraft.ports) != 2 || s.containerDraft.ports[1].host != "3" {
		t.Fatal("remaining port rows changed")
	}
}

func TestCredentialLookupErrorsAreVisibleAndMissingCredentialsAllowAnonymousPulls(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"credsStore":"test"}`), 0600); err != nil {
		t.Fatal(err)
	}
	store := &registryStore{path: path, helper: func(context.Context, string, string, []byte) ([]byte, error) {
		return nil, errors.New("helper unavailable")
	}}
	if _, _, err := store.find(t.Context(), dockerHub); err == nil {
		t.Fatal("helper failure disappeared")
	}
	store.helper = func(context.Context, string, string, []byte) ([]byte, error) { return nil, errRegistryNotFound }
	if _, found, err := store.find(t.Context(), dockerHub); err != nil || found {
		t.Fatalf("anonymous lookup: %v %v", found, err)
	}
}

func TestKubernetesStatusPriorities(t *testing.T) {
	cases := []struct{ input, want string }{
		{`{"status":{"phase":"Pending","containerStatuses":[{"state":{"waiting":{"reason":"ContainerCreating"}}}]}}`, "ContainerCreating"},
		{`{"status":{"phase":"Succeeded","containerStatuses":[{"state":{"terminated":{"reason":"Completed","exitCode":0}}}]}}`, "Completed"},
		{`{"spec":{"initContainers":[{},{}]},"status":{"initContainerStatuses":[{"state":{"terminated":{"exitCode":0}}},{"state":{"running":{}}}]}}`, "Init:1/2"},
		{`{"status":{"initContainerStatuses":[{"state":{"terminated":{"exitCode":2}}}]}}`, "Init:ExitCode:2"},
		{`{"metadata":{"deletionTimestamp":"now"},"status":{"phase":"Running"}}`, "Terminating"},
	}
	for _, tc := range cases {
		var o kubeObject
		if err := json.Unmarshal([]byte(tc.input), &o); err != nil {
			t.Fatal(err)
		}
		if got := o.status(); got != tc.want {
			t.Errorf("status %s, want %s", got, tc.want)
		}
	}
}

func TestPodDeletionUsesForegroundRequestAndValidatesNames(t *testing.T) {
	f, s := newFakeEngine(t)
	k := installKube(t, f)
	var pod kubeObject
	pod.Kind = "Pod"
	pod.Metadata.Namespace = "default"
	pod.Metadata.Name = "web-1"
	s.deletePod(pod)
	k.mu.Lock()
	commands := append([][]string{}, k.commands...)
	background := append([]bool{}, k.background...)
	k.mu.Unlock()
	found := false
	for i, cmd := range commands {
		if len(cmd) > 5 && cmd[5] == "delete" {
			found = true
			if background[i] || !reflect.DeepEqual(cmd[5:], []string{"delete", "pod", "-n", "default", "web-1", "--wait=false"}) {
				t.Fatalf("delete command %v background=%v", cmd, background[i])
			}
		}
	}
	if !found {
		t.Fatal("no deletion request")
	}
	before := len(commands)
	pod.Metadata.Name = "--all"
	s.deletePod(pod)
	k.mu.Lock()
	defer k.mu.Unlock()
	if len(k.commands) != before {
		t.Fatal("invalid name reached kubectl")
	}
}

func TestAdvancedControlsFitTheMinimumWindow(t *testing.T) {
	_, s := newFakeEngine(t)
	s.selectRow("c-api")
	s.tab[containersPage] = 4
	s.dockMax = true
	tt := ui.NewTester(s.view, 860, 520)
	for _, name := range []string{"Container settings", "Close", "Save environment", "Save ports"} {
		rect, ok := tt.Find(name)
		if !ok || rect.X < 0 || rect.X+rect.W > 860 {
			t.Errorf("%s lies outside the minimum window: %+v present=%v", name, rect, ok)
		}
	}
}
