package main

import (
	"archive/tar"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"image/png"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"
)

// fakeEngine answers the reads the window makes with a small, fixed engine
// and records every request that would change it.
type fakeEngine struct {
	mu      sync.Mutex
	changes []string
	last    string
	server  *httptest.Server
	// shell makes an interactive exec run a real shell on this machine,
	// for the preview engine of TestServeFakeEngine; proc is the latest.
	shell bool
	proc  *os.Process
	// execCmd is the command of the exec created last.
	execCmd []string
}

// fakeDirs and fakeFiles are the small filesystem the fake containers
// share: each directory's entries ("/" ends a folder, "@" marks a link),
// and each file's content.
var (
	fakeDirs = map[string][]string{
		"/":                  {"app/", "bin/", "etc/"},
		"/app/":              {"dist/", "node_modules/", "package.json", ".env", "@server.js"},
		"/app/dist/":         {"server.js"},
		"/app/node_modules/": {},
		"/bin/":              {"sh", "@ls"},
		"/etc/":              {"nginx/", "hostname", "os-release"},
		"/etc/nginx/":        {"nginx.conf"},
	}
	fakeFiles = map[string]string{
		"/app/package.json":     "{\n  \"name\": \"shop-api\",\n  \"version\": \"1.4.0\",\n  \"scripts\": { \"start\": \"node dist/server.js\" }\n}\n",
		"/app/.env":             "NODE_ENV=development\nPORT=3000\n",
		"/app/server.js":        "require('./dist/server.js')\n",
		"/app/dist/server.js":   "const http = require('http')\nhttp.createServer((req, res) => res.end('ok')).listen(3000)\n",
		"/bin/sh":               "\x7fELF\x02\x01\x01\x00binary",
		"/bin/ls":               "\x7fELF\x02\x01\x01\x00binary",
		"/etc/hostname":         "c-api\n",
		"/etc/os-release":       "NAME=\"Alpine Linux\"\nVERSION_ID=3.21.0\n",
		"/etc/nginx/nginx.conf": "worker_processes auto;\nerror_log /var/log/nginx/error.log warn;\nevents { worker_connections 1024; }\n",
	}
)

// fakeExec answers an exec as a container would: the Files tab's listing
// and preview scripts from fakeDirs and fakeFiles, anything else with the
// root listing ls -1Ap prints.
func fakeExec(cmd []string) string {
	if len(cmd) == 5 && cmd[2] == listScript {
		var out strings.Builder
		for _, e := range fakeDirs[cmd[4]] {
			name := strings.TrimPrefix(e, "@")
			if name != e {
				fmt.Fprintf(&out, "L|%s\n", name)
			}
			if strings.HasSuffix(name, "/") {
				fmt.Fprintf(&out, "S|directory|4096|1794000000|%s\n", strings.TrimSuffix(name, "/"))
			} else {
				fmt.Fprintf(&out, "S|regular file|%d|1794000000|%s\n", len(fakeFiles[cmd[4]+name]), name)
			}
		}
		return out.String()
	}
	if len(cmd) == 5 && cmd[2] == previewScript {
		return fakeFiles[cmd[4]]
	}
	return "app/\nbin/\netc/\npackage.json\n"
}

// body is what the latest change sent.
func (f *fakeEngine) body() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.last
}

func (f *fakeEngine) changed() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.changes...)
}

// newFakeEngine starts the engine and returns a window state reading from it.
func newFakeEngine(t *testing.T) (*fakeEngine, *desktop) {
	t.Helper()
	f := &fakeEngine{}
	now := time.Now().Unix()
	containers := fmt.Sprintf(`[
	 {"Id":"c-web","Names":["/shop-web-1"],"Image":"nginx:1.27-alpine","ImageID":"sha256:img-nginx","State":"running","Created":%[1]d,"KiriStartedAt":%[2]d,
	  "Labels":{"com.docker.compose.project":"shop","com.docker.compose.service":"web","com.docker.compose.project.working_dir":"SHOP_DIR","com.docker.compose.project.config_files":"SHOP_DIR/compose.yaml","com.docker.compose.version":"2.32.1"},"Ports":[{"Type":"tcp","PrivatePort":80,"PublicPort":8080},{"IP":"::","Type":"tcp","PrivatePort":80,"PublicPort":8080}],
	  "NetworkSettings":{"Networks":{"shop_default":{"IPAddress":"172.18.0.2"}}}},
	 {"Id":"c-api","Names":["/shop-api-1"],"Image":"shop/api:dev","ImageID":"sha256:img-api","State":"running","Created":%[1]d,"KiriStartedAt":%[2]d,
	  "Labels":{"com.docker.compose.project":"shop","com.docker.compose.service":"api","com.docker.compose.project.working_dir":"SHOP_DIR","com.docker.compose.project.config_files":"SHOP_DIR/compose.yaml","com.docker.compose.version":"2.32.1"},"Ports":[{"Type":"tcp","PrivatePort":3000,"PublicPort":3000}],
	  "NetworkSettings":{"Networks":{"shop_default":{"IPAddress":"172.18.0.3"}}},"Mounts":[{"Type":"volume","Name":"shop_pgdata","Destination":"/data"}]},
	 {"Id":"c-loki","Names":["/loki"],"Image":"grafana/loki:3.2","ImageID":"sha256:img-loki","State":"exited","Created":%[1]d,"KiriFinishedAt":%[3]d,"KiriExitCode":1,
	  "Labels":{"com.docker.compose.project":"observability"}},
	 {"Id":"c-redis","Names":["/redis-cache"],"Image":"redis:7.4-alpine","ImageID":"sha256:img-redis","State":"running","Created":%[1]d,"KiriStartedAt":%[1]d,
	  "Ports":[{"Type":"tcp","PrivatePort":6379,"PublicPort":6379}],"NetworkSettings":{"Networks":{"bridge":{"IPAddress":"172.17.0.2"}}}},
	 {"Id":"c-job","Names":["/db-migrate"],"Image":"shop/migrate:dev","ImageID":"sha256:img-migrate","State":"exited","Created":%[1]d,"KiriFinishedAt":%[1]d,"KiriExitCode":0}
	]`, now-3*86400, now-240, now-360)
	// The shop project's Compose file is real, so its Config tab has
	// something to show and to save.
	shop := t.TempDir()
	compose := "name: shop\nservices:\n  api:\n    image: shop/api:dev\n    ports:\n      - \"3000:3000\"\n    volumes:\n      - pgdata:/data\n" +
		"  web:\n    image: nginx:1.27-alpine\n    ports:\n      - \"8080:80\"\nvolumes:\n  pgdata:\n"
	if err := os.WriteFile(filepath.Join(shop, "compose.yaml"), []byte(compose), 0o644); err != nil {
		t.Fatal(err)
	}
	containers = strings.ReplaceAll(containers, "SHOP_DIR", filepath.ToSlash(shop))
	// Each stats read moves the counters on, so the second refresh has a
	// delta to turn into a CPU share.
	reads := map[string]uint64{}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		if r.Method != http.MethodGet {
			sent, _ := io.ReadAll(r.Body)
			f.mu.Lock()
			f.last = string(sent)
			f.changes = append(f.changes, r.Method+" "+r.URL.RequestURI())
			f.mu.Unlock()
			if path == "/containers/create" {
				w.WriteHeader(http.StatusCreated)
				fmt.Fprint(w, `{"Id":"c-new"}`)
			}
			if strings.HasSuffix(path, "/exec") {
				var spec struct{ Cmd []string }
				_ = json.Unmarshal(sent, &spec)
				f.mu.Lock()
				f.execCmd = spec.Cmd
				f.mu.Unlock()
				w.WriteHeader(http.StatusCreated)
				fmt.Fprint(w, `{"Id":"exec-1"}`)
			}
			// An exec answers as the engine does: it upgrades the
			// connection and writes one multiplexed frame.
			f.mu.Lock()
			shell := f.shell
			f.mu.Unlock()
			if path == "/exec/exec-1/start" && shell && strings.Contains(string(sent), `"Tty":true`) {
				if conn, buf, err := w.(http.Hijacker).Hijack(); err == nil {
					buf.WriteString("HTTP/1.1 101 UPGRADED\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n")
					buf.Flush()
					f.serveShell(conn, buf.Reader)
				}
				return
			}
			if path == "/kiri/exec/exec-1/stop" {
				f.mu.Lock()
				if f.proc != nil {
					_ = f.proc.Kill()
				}
				f.mu.Unlock()
			}
			if path == "/exec/exec-1/start" {
				if conn, buf, err := w.(http.Hijacker).Hijack(); err == nil {
					f.mu.Lock()
					out := fakeExec(f.execCmd)
					f.mu.Unlock()
					buf.WriteString("HTTP/1.1 101 UPGRADED\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n")
					header := []byte{1, 0, 0, 0, 0, 0, 0, 0}
					binary.BigEndian.PutUint32(header[4:], uint32(len(out)))
					buf.Write(header)
					buf.WriteString(out)
					buf.Flush()
					conn.Close()
				}
			}
			return
		}
		if r.Header.Get("X-Kiri-Background") != "1" {
			t.Errorf("read could keep the VM awake: %s", r.URL)
		}
		switch {
		case strings.HasSuffix(path, "/archive"):
			// One file, as a tar, as the engine sends it.
			name := r.URL.Query().Get("path")
			content, ok := fakeFiles[name]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				fmt.Fprint(w, `{"message":"no such file"}`)
				return
			}
			archive := tar.NewWriter(w)
			_ = archive.WriteHeader(&tar.Header{Name: filepath.Base(name), Mode: 0o644, Size: int64(len(content)), Typeflag: tar.TypeReg})
			_, _ = archive.Write([]byte(content))
			_ = archive.Close()
		case path == "/exec/exec-1/json":
			fmt.Fprint(w, `{"Running":false,"ExitCode":0}`)
		case path == "/containers/kiri-k8s/json":
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"message":"No such container"}`)
		case path == "/info":
			fmt.Fprint(w, `{"Name":"kiri","KiriExecStop":true,"ServerVersion":"0.4.0","KiriVmAwake":true,"KiriVMControls":true,"NCPU":4,"MemTotal":8589934592,"Containers":5,"ContainersRunning":3,"Images":3}`)
		case path == "/containers/json":
			fmt.Fprint(w, containers)
		case path == "/images/json":
			fmt.Fprintf(w, `[{"Id":"sha256:img-nginx","RepoTags":["nginx:1.27-alpine"],"Size":50331648,"Created":%[1]d},
			 {"Id":"sha256:img-api","RepoTags":["shop/api:dev"],"Size":432013312,"Created":%[1]d},
			 {"Id":"sha256:img-alpine","RepoTags":["alpine:3.21"],"Size":8703180,"Created":%[1]d}]`, now-7200)
		case path == "/volumes":
			fmt.Fprint(w, `{"Volumes":[{"Name":"shop_pgdata","Driver":"local","Mountpoint":"/var/lib/kiri/volumes/shop_pgdata/_data","CreatedAt":"2026-11-01T10:00:00Z"},
			 {"Name":"scratch","Driver":"local","Mountpoint":"/var/lib/kiri/volumes/scratch/_data","CreatedAt":"2026-10-01T10:00:00Z"}]}`)
		case path == "/networks":
			fmt.Fprint(w, `[{"Id":"n-shop","Name":"shop_default","Driver":"bridge","Created":"2026-11-01T10:00:00Z","IPAM":{"Config":[{"Subnet":"172.18.0.0/16","Gateway":"172.18.0.1"}]}},
			 {"Id":"n-bridge","Name":"bridge","Driver":"bridge","IPAM":{"Config":[{"Subnet":"172.17.0.0/16","Gateway":"172.17.0.1"}]}},
			 {"Id":"n-host","Name":"host","Driver":"host"},{"Id":"n-none","Name":"none","Driver":"null"}]`)
		case path == "/kiri/resources":
			fmt.Fprint(w, `{"cpus":4,"memory_mib":8192,"running_cpus":4,"running_memory_mib":8192,"awake":true,"max_cpus":10,"max_memory_mib":32768}`)
		case strings.HasSuffix(path, "/stats"):
			f.mu.Lock()
			reads[path]++
			n := reads[path]
			f.mu.Unlock()
			share := uint64(len(path)%5+1) * 30_000_000
			fmt.Fprintf(w, `{"cpu_stats":{"cpu_usage":{"total_usage":%d},"system_cpu_usage":%d,"online_cpus":4},"memory_stats":{"usage":%d,"stats":{"inactive_file":1048576}}}`,
				n*share, n*4_000_000_000, uint64(len(path))*9_000_000)
		case strings.HasSuffix(path, "/logs"):
			fmt.Fprint(w, "server listening on :3000\nconnected to postgres shop-db-1:5432\nGET /api/products 200 12ms\n"+
				"WARN slow query 412ms SELECT * FROM orders WHERE user_id = $1; consider an index on orders(user_id) so that checkout stays fast under load\n"+
				"debug cache hit ratio 0.92\nERROR payment gateway timed out after 5000ms\nPOST /api/checkout 201 88ms\n")
		case strings.HasPrefix(path, "/containers/") && strings.HasSuffix(path, "/json"):
			fmt.Fprint(w, `{"Id":"c-api-full-id","Path":"node","Args":["dist/server.js"],"Config":{"Image":"shop/api:dev","Tty":true,"WorkingDir":"/app","Env":["NODE_ENV=development"]}}`)
		case path == "/events":
			fmt.Fprintf(w, `{"Type":"container","Action":"start","Actor":{"Attributes":{"name":"shop-api-1"}},"time":%d}`+"\n"+
				`{"Type":"container","Action":"die","Actor":{"Attributes":{"name":"loki"}},"time":%d}`+"\n", now-240, now-360)
		case path == "/system/df":
			fmt.Fprint(w, `{"LayersSize":3113851290,"Containers":[{"SizeRw":325058560}],"Volumes":[{"Name":"shop_pgdata","UsageData":{"Size":432013312}},{"Name":"scratch","UsageData":{"Size":4096}}],"BuildCache":[{"Size":2254857830}]}`)
		case path == "/kiri/images/layers":
			fmt.Fprint(w, `{"images":[{"layers":[{"by":"ADD rootfs.tar.xz /","size":8703180},{"by":"RUN apk add --no-cache nodejs","size":212013312},{"by":"CMD [\"node\"]","size":0,"empty":true}]}]}`)
		case path == "/kiri/images/scan":
			fmt.Fprint(w, `{"scanned":1,"packages":120,"vulns":[{"id":"CVE-2026-1001","severity":"Medium","package":"zlib","version":"1.3.0","fixed":["1.3.1"]},
			 {"id":"CVE-2026-2002","severity":"High","package":"openssl","version":"3.3.0","fixed":[]}]}`)
		case path == "/kiri/builder":
			fmt.Fprint(w, `{"cache_gb":20,"default_cache_gb":20,"cache_limit":21474836480,"idle_secs":600,"image":"moby/buildkit:v0.17.0","state":"running"}`)
		case path == "/kiri/domains":
			fmt.Fprint(w, `{"http":{"port":80,"default":80,"running":true,"serving":true},"https":{"port":443,"default":443,"running":true,"serving":true}}`)
		case path == "/kiri/proxy":
			fmt.Fprint(w, `{"settings":{"mode":"system","http":"","https":"","no_proxy":"","containers":false},"effective":{"http":"http://127.0.0.1:7890","https":"http://127.0.0.1:7890","no_proxy":["localhost","*.internal"],"pac":false}}`)
		case path == "/version":
			fmt.Fprint(w, `{"Version":"0.4.0","ApiVersion":"1.47","Os":"linux","Arch":"arm64","GoVersion":"go1.27.3"}`)
		default:
			fmt.Fprint(w, `{}`)
		}
	}))
	t.Cleanup(f.server.Close)
	s := newDesktop(newEngineClient(func(ctx context.Context) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", f.server.Listener.Addr().String())
	}))
	s.credentials = &registryStore{path: filepath.Join(t.TempDir(), "config.json"), native: &memoryRegistry{items: map[string]registryLogin{}}, helper: runCredentialHelper}
	s.prefsFile = filepath.Join(t.TempDir(), "desktop.json")
	s.prefs.Theme, s.prefs.Lang, s.contextName = "dark", "en", "kiri-dev"
	s.refresh()
	s.refresh()
	t.Cleanup(s.closeInteractive)
	return f, s
}

func has(tt *ui.Tester, labels ...string) (missing string) {
	for _, l := range labels {
		if _, found := tt.Find(l); !found {
			return l
		}
	}
	return ""
}

// Compose members sit under their project's heading, and folding the
// heading hides them without touching the other rows.
func TestContainersGroupUnderTheirComposeProject(t *testing.T) {
	_, s := newFakeEngine(t)
	tt := ui.NewTester(s.view, 1180, 760)
	if m := has(tt, "shop", "Compose · 2/2 running", "Details for shop-api-1", "Details for redis-cache", "Exited (1) · 6 min ago"); m != "" {
		t.Fatalf("missing %q in %q", m, tt.Texts())
	}
	if err := tt.Click("Fold shop"); err != nil {
		t.Fatal(err)
	}
	if _, found := tt.Find("Details for shop-api-1"); found {
		t.Fatal("folding left a member visible")
	}
	if _, found := tt.Find("Details for redis-cache"); !found {
		t.Fatal("folding hid a container outside the project")
	}
}

// Ticking a project picks its members; the bar that replaces the filters
// then stops or deletes them all, forcing the running ones on delete.
func TestPickedContainersAreActedOnTogether(t *testing.T) {
	f, s := newFakeEngine(t)
	tt := ui.NewTester(s.view, 1180, 760)
	if err := tt.Click("Select shop"); err != nil {
		t.Fatal(err)
	}
	if !s.picked["c-api"] || !s.picked["c-web"] || len(s.picked) != 2 || !tt.HasText("2 selected") {
		t.Fatalf("picked %v, texts %q", s.picked, tt.Texts())
	}
	if err := tt.Click("Stop"); err != nil {
		t.Fatal(err)
	}
	changes := strings.Join(f.changed(), " ")
	if !strings.Contains(changes, "/containers/c-api/stop") || !strings.Contains(changes, "/containers/c-web/stop") {
		t.Fatalf("stop: %s", changes)
	}
	// Every shown container, from the header's box.
	if err := tt.Click("Select all"); err != nil || len(s.picked) != len(s.containers) {
		t.Fatalf("select all: %v %v", s.picked, err)
	}
	if err := tt.Click("Delete"); err != nil || s.pending == nil {
		t.Fatalf("delete asked nothing: %v", err)
	}
	s.pending.run()
	changes = strings.Join(f.changed(), " ")
	if !strings.Contains(changes, "DELETE /containers/c-api?v=0&force=1") || !strings.Contains(changes, "DELETE /containers/c-job?v=0") {
		t.Fatalf("delete: %s", changes)
	}
	if len(s.picked) != 0 {
		t.Fatalf("deleted containers stayed picked: %v", s.picked)
	}
}

// A project's heading opens its own panel, with what its members share.
func TestComposeProjectOpensItsPanel(t *testing.T) {
	_, s := newFakeEngine(t)
	tt := ui.NewTester(s.view, 1180, 760)
	if err := tt.Click("Details for shop"); err != nil || s.selected[containersPage] != projectKey("shop") {
		t.Fatalf("selected %q: %v", s.selected[containersPage], err)
	}
	if m := has(tt, "Compose services", "api, web", "2.32.1", "2/2 running", "shop_default", "Stop all in shop"); m != "" {
		t.Fatalf("missing %q in %q", m, tt.Texts())
	}
}

// The Config tab reads a project's Compose file where it lives, and Save
// writes the edit back, keeping the file's permissions.
func TestComposeFileIsEditedInPlace(t *testing.T) {
	_, s := newFakeEngine(t)
	file := filepath.Join(t.TempDir(), "compose.yaml")
	original := "services:\n  web:\n    image: nginx\n"
	if err := os.WriteFile(file, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	for i := range s.containers {
		if s.containers[i].Project == "shop" {
			s.containers[i].ConfigFiles = file
		}
	}
	s.selectRow(projectKey("shop"))
	s.tab[containersPage+"/project"] = 1
	tt := ui.NewTester(s.view, 1180, 760)
	tt.Frame()
	if !s.compose.loaded || s.compose.text != original || !tt.HasText(file) {
		t.Fatalf("loaded %v %q, texts %q", s.compose.loaded, s.compose.text, tt.Texts())
	}
	s.compose.text += "    restart: always\n"
	tt.Frame()
	if err := tt.Click("Save"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(file)
	info, _ := os.Stat(file)
	if err != nil || string(data) != original+"    restart: always\n" || info.Mode().Perm() != 0o600 {
		t.Fatalf("saved %q (%v), mode %v", data, err, info.Mode().Perm())
	}
}

// A narrow window folds the sidebar without touching the saved choice;
// Command-B then unfolds it only until the window widens again.
func TestNarrowWindowFoldsTheSidebar(t *testing.T) {
	_, s := newFakeEngine(t)
	tt := ui.NewTester(s.view, 900, 700)
	tt.Frame()
	if side, found := tt.Find("Images"); !found || side.W > 40 || s.prefs.NavCollapsed {
		t.Fatalf("narrow window: %v %v, saved %v", side, found, s.prefs.NavCollapsed)
	}
	// The table drops its optional columns; the names stay readable.
	if _, found := tt.Find("Ports"); found || !tt.HasText("shop-api-1") {
		t.Fatalf("narrow table: %q", tt.Texts())
	}
	tt.Key(ui.Cmd, ui.KeyB)
	if side, _ := tt.Find("Images"); side.W < 150 || s.prefs.NavCollapsed {
		t.Fatalf("peek: %v, saved %v", side, s.prefs.NavCollapsed)
	}
	tt.SetSize(1180, 760)
	tt.Frame()
	if side, _ := tt.Find("Images"); side.W < 150 {
		t.Fatalf("wide window stayed folded: %v", side)
	}
}

// At every window width, with the detail panel open or not, the container
// table's cells stay clear of the row's buttons, the name keeps its room
// and the toolbar's main action stays in view.
func TestContainerTableFitsEveryWidth(t *testing.T) {
	_, s := newFakeEngine(t)
	for _, sel := range []string{"", "c-api"} {
		s.selectRow(sel)
		for w := 860; w <= 1500; w += 20 {
			s.lists = map[string]*ui.ListState{}
			tt := ui.NewTester(s.view, w, 760)
			tt.Frame()
			name, _ := tt.Find("shop-api-1")
			restart, found := tt.Find("Restart shop-api-1")
			if !found || name.W < 60 || name.X+name.W > restart.X {
				t.Fatalf("width %v, panel %q: name %v, restart %v", w, sel, name, restart)
			}
			if cpu, shown := tt.Find("12.0%"); shown && cpu.X+cpu.W > restart.X {
				t.Fatalf("width %v, panel %q: CPU %v runs into %v", w, sel, cpu, restart)
			}
			run, found := tt.Find("Run container")
			card := float32(w)
			if sel != "" {
				card -= s.dockWidth()
			}
			if !found || run.X+run.W > card {
				t.Fatalf("width %v, panel %q: Run container at %v", w, sel, run)
			}
		}
	}
}

// A line's level is the first marking word in it: a warning that speaks
// of a failure is a warning.
func TestLogLinesAreToldApartByLevel(t *testing.T) {
	for line, want := range map[string]int{
		"1:M 13 Nov # WARNING Memory overcommit must be enabled! ... may fail under low memory": logWarn,
		"level=error msg=\"dial tcp: refused\"":                                                 logError,
		"panic: runtime error: index out of range":                                              logError,
		"2026-11-14 INFO server started":                                                        logInfo,
		"[debug] cache hit":                                                                     logDebug,
		"GET /api/products 200 12ms":                                                            logAny,
	} {
		if got := logLevel(line); got != want {
			t.Errorf("%q: level %d, want %d", line, got, want)
		}
	}
}

// Clearing a stream drops what was read and numbers what follows from one;
// lines let go to bound the buffer keep the others' numbers.
func TestClearedLogsNumberFromOne(t *testing.T) {
	l := &liveLogs{}
	l.append([]byte("a\nb\n"))
	l.clear()
	l.append([]byte("c\n"))
	if got := l.snapshot(); len(got.lines) != 1 || got.lines[0] != "c" || got.first != 1 {
		t.Fatalf("after clear: %+v", got)
	}
	l.append([]byte(strings.Repeat("x\n", maxLiveLogLines)))
	if got := l.snapshot(); got.first != 2 || len(got.lines) != maxLiveLogLines {
		t.Fatalf("bounded: first %d, %d lines", got.first, len(got.lines))
	}
}

// The Files tab lists a folder with its folders first, opens folders by
// their rows and the path's parts, previews a text file, says a binary
// one cannot be shown, and saves a file to Downloads.
func TestFilesTabBrowsesPreviewsAndSaves(t *testing.T) {
	_, s := newFakeEngine(t)
	t.Setenv("HOME", t.TempDir())
	s.selectRow("c-api")
	s.tab[containersPage] = 3
	tt := ui.NewTester(s.view, 1180, 760)
	tt.Frame()
	click(t, tt, "Open app")
	if m := has(tt, "dist", "node_modules", "package.json", ".env", "server.js", "↪"); m != "" {
		t.Fatalf("missing %q in %q", m, tt.Texts())
	}
	if names := []string{s.detail.files[0].name, s.detail.files[1].name}; names[0] != "dist" || names[1] != "node_modules" {
		t.Fatalf("folders first: %v", s.detail.files)
	}
	click(t, tt, "Open package.json")
	tt.Frame()
	if !tt.HasText(`  "name": "shop-api",`) {
		t.Fatalf("preview: %q", tt.Texts())
	}
	click(t, tt, "Save to Downloads")
	saved, err := os.ReadFile(filepath.Join(os.Getenv("HOME"), "Downloads", "package.json"))
	if err != nil || string(saved) != fakeFiles["/app/package.json"] {
		t.Fatalf("saved %q: %v", saved, err)
	}
	// The root part of the path goes back to the top.
	click(t, tt, "Open /")
	click(t, tt, "Open bin")
	click(t, tt, "Open sh")
	tt.Frame()
	if !tt.HasText("This file is binary or larger than 256 KB; save it to Downloads to open it.") {
		t.Fatalf("binary: %q", tt.Texts())
	}
}

// A port published on IPv4 and IPv6 arrives twice and is shown once.
func TestContainerPortsAreListedOnce(t *testing.T) {
	_, s := newFakeEngine(t)
	if c, _ := s.container("c-web"); len(c.Ports) != 1 || c.Ports[0] != (portMap{80, 8080, "tcp"}) {
		t.Fatalf("ports: %+v", c.Ports)
	}
}

func TestStopAndStartReachTheEngine(t *testing.T) {
	f, s := newFakeEngine(t)
	tt := ui.NewTester(s.view, 1180, 760)
	if err := tt.Click("Stop shop-web-1"); err != nil {
		t.Fatal(err)
	}
	if err := tt.Click("Start db-migrate"); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(f.changed(), ", ")
	if got != "POST /containers/c-web/stop, POST /containers/c-job/start" {
		t.Fatalf("requests: %s", got)
	}
}

// Deleting asks first; a running container is removed by force, and
// cancelling sends nothing.
func TestDeleteNeedsConfirmation(t *testing.T) {
	f, s := newFakeEngine(t)
	tt := ui.NewTester(s.view, 1180, 760)
	if err := tt.Click("More actions for redis-cache"); err != nil {
		t.Fatal(err)
	}
	if err := tt.ChooseMenuItem("Delete…"); err != nil {
		t.Fatal(err)
	}
	if m := has(tt, "Delete container redis-cache?", "The container is running and will be force-stopped."); m != "" {
		t.Fatalf("missing %q", m)
	}
	if err := tt.Click("Cancel"); err != nil {
		t.Fatal(err)
	}
	if s.pending != nil || len(f.changed()) != 0 {
		t.Fatalf("cancel still acted: %v", f.changed())
	}
	s.selectRow("c-redis")
	tt.Frame()
	if err := tt.Click("Delete redis-cache"); err != nil {
		t.Fatal(err)
	}
	if err := tt.Click("Delete"); err != nil {
		t.Fatal(err)
	}
	if got := f.changed(); len(got) != 1 || got[0] != "DELETE /containers/c-redis?v=0&force=1" {
		t.Fatalf("requests: %v", got)
	}
}

func TestSearchCombinesWithTheStateFilter(t *testing.T) {
	_, s := newFakeEngine(t)
	tt := ui.NewTester(s.view, 1180, 760)
	tt.Key(ui.Cmd, ui.KeyK)
	tt.Type("shop")
	if _, found := tt.Find("Details for redis-cache"); found || s.search != "shop" {
		t.Fatalf("search did not narrow the list: %q", s.search)
	}
	if err := tt.Click("Stopped"); err != nil {
		t.Fatal(err)
	}
	if _, found := tt.Find("Details for shop-api-1"); found {
		t.Fatal("the stopped filter kept a running container")
	}
	if _, found := tt.Find("Details for db-migrate"); !found {
		t.Fatal("the matching stopped container disappeared")
	}
}

// The dock follows the selection: logs by default, then the inspect fields.
func TestDockShowsTheSelectedContainer(t *testing.T) {
	_, s := newFakeEngine(t)
	tt := ui.NewTester(s.view, 1180, 760)
	if err := tt.Click("Details for shop-api-1"); err != nil {
		t.Fatal(err)
	}
	waitDesktop(t, tt, func() bool { return tt.HasText("server listening on :3000") })
	if !tt.HasText("server listening on :3000") {
		t.Fatalf("logs missing: %q", tt.Texts())
	}
	// The level picker keeps one level.
	s.logLevel = logError
	tt.Frame()
	if tt.HasText("server listening on :3000") || !tt.HasText("ERROR payment gateway timed out after 5000ms") {
		t.Fatalf("errors only: %q", tt.Texts())
	}
	s.logLevel = logAny
	if err := tt.Click("Info"); err != nil {
		t.Fatal(err)
	}
	if m := has(tt, "node dist/server.js", "localhost:3000 → 3000/tcp", "shop_pgdata → /data"); m != "" {
		t.Fatalf("missing %q in %q", m, tt.Texts())
	}
	if err := tt.Click("Close"); err != nil || s.selected[containersPage] != "" {
		t.Fatalf("close left the dock open: %v", err)
	}
}

func TestArrowKeysMoveTheSelection(t *testing.T) {
	_, s := newFakeEngine(t)
	tt := ui.NewTester(s.view, 1180, 760)
	tt.Key(0, ui.KeyDown)
	first := s.selected[containersPage]
	tt.Key(0, ui.KeyDown)
	if second := s.selected[containersPage]; first == "" || second == "" || first == second {
		t.Fatalf("selection did not move: %q then %q", first, second)
	}
	tt.Key(0, ui.KeyEscape)
	if s.selected[containersPage] != "" {
		t.Fatal("Escape kept the selection")
	}
}

// Folded, the sidebar shows icons only; the engine's state moves to the
// title bar so that it stays visible.
func TestSidebarFoldsToIcons(t *testing.T) {
	_, s := newFakeEngine(t)
	tt := ui.NewTester(s.view, 1180, 760)
	side, _ := tt.Find("Images")
	if m := has(tt, "Engine running"); m != "" || side.W < 150 {
		t.Fatalf("open sidebar: missing %q, entry %v", m, side)
	}
	// The sidebar lists the pages without section titles.
	if _, found := tt.Find("Docker"); found {
		t.Fatal("the open sidebar shows a section title")
	}
	tt.Key(ui.Cmd, ui.KeyB)
	side, found := tt.Find("Images")
	if !found || side.W > 40 || !s.prefs.NavCollapsed {
		t.Fatalf("folded entry: %v %v", side, found)
	}
	if !tt.HasText("Engine running · 4 CPU") {
		t.Fatalf("engine state left the window: %q", tt.Texts())
	}
	if err := tt.Click("Images"); err != nil || s.page != imagesPage {
		t.Fatalf("folded entry did not navigate: %v", err)
	}
}

func TestImagesInUseCannotBeDeleted(t *testing.T) {
	f, s := newFakeEngine(t)
	s.navigate(imagesPage)
	tt := ui.NewTester(s.view, 1180, 760)
	if err := tt.Click("Delete nginx:1.27-alpine"); err == nil && s.pending != nil {
		t.Fatal("an image in use offered removal")
	}
	if err := tt.Click("Delete alpine:3.21"); err != nil {
		t.Fatal(err)
	}
	if err := tt.Click("Delete"); err != nil {
		t.Fatal(err)
	}
	if got := f.changed(); len(got) != 1 || got[0] != "DELETE /images/alpine:3.21" {
		t.Fatalf("requests: %v", got)
	}
}

func TestImageDockShowsLayersAndStoredScan(t *testing.T) {
	_, s := newFakeEngine(t)
	s.navigate(imagesPage)
	tt := ui.NewTester(s.view, 1180, 760)
	if err := tt.Click("Details for shop/api:dev"); err != nil {
		t.Fatal(err)
	}
	if m := has(tt, "Layers 3", "RUN apk add --no-cache nodejs", "1 high · 1 med"); m != "" {
		t.Fatalf("missing %q in %q", m, tt.Texts())
	}
	// "Security" also titles a table column, so the tab is chosen directly.
	s.tab[imagesPage] = 1
	tt.Frame()
	// The worst finding leads, whatever order the scan listed them in.
	high, _ := tt.Find("CVE-2026-2002")
	medium, _ := tt.Find("CVE-2026-1001")
	if high.H == 0 || medium.H == 0 || high.Y >= medium.Y {
		t.Fatalf("findings out of order: %v %v", high, medium)
	}
}

func TestRunDialogValidatesBeforeSending(t *testing.T) {
	f, s := newFakeEngine(t)
	tt := ui.NewTester(s.view, 1180, 760)
	// fill opens the dialog anew, which starts in its image field, and
	// types into the fields in Tab order: image, name, host port,
	// container port. An empty value skips its field.
	fill := func(values ...string) {
		t.Helper()
		if s.form.kind != "" {
			if err := tt.Click("Cancel"); err != nil {
				t.Fatal(err)
			}
		}
		if err := tt.Click("Run container"); err != nil {
			t.Fatal(err)
		}
		for i, v := range values {
			if i > 0 {
				tt.Key(0, ui.KeyTab)
			}
			tt.Type(v)
		}
		if err := tt.Click("Run"); err != nil {
			t.Fatal(err)
		}
	}
	fill()
	if _, found := tt.Find("Enter an image"); !found || s.form.kind != "run" || len(f.changed()) != 0 {
		t.Fatalf("an empty form was sent: %v", f.changed())
	}
	fill("alpine:3.21", "", "8080")
	if _, found := tt.Find("Both ports must be numbers from 1 to 65535"); !found {
		t.Fatalf("a half-filled port mapping was accepted: %+v", s.form)
	}
	fill("alpine:3.21", "", "8080", "80")
	if got := f.changed(); s.form.kind != "" || len(got) != 2 || got[0] != "POST /containers/create" || got[1] != "POST /containers/c-new/start" {
		t.Fatalf("requests: %v, form %+v", got, s.form)
	}
}

func TestBuiltInNetworksCannotBeDeleted(t *testing.T) {
	_, s := newFakeEngine(t)
	s.navigate(networksPage)
	tt := ui.NewTester(s.view, 1180, 760)
	for _, name := range []string{"bridge", "host", "none", "shop_default"} {
		_ = tt.Click("Delete " + name)
		if s.pending != nil {
			t.Fatalf("%s offered removal", name)
		}
	}
}

func TestStoppedEngineOffersToStart(t *testing.T) {
	f, s := newFakeEngine(t)
	s.info.KiriStopped = true
	tt := ui.NewTester(s.view, 1180, 760)
	if m := has(tt, "The engine is stopped", "Engine stopped"); m != "" {
		t.Fatalf("missing %q", m)
	}
	if err := tt.Click("Start engine"); err != nil {
		t.Fatal(err)
	}
	if got := f.changed(); len(got) != 1 || got[0] != "POST /kiri/wake" {
		t.Fatalf("requests: %v", got)
	}
}

// Appearance choices apply to the frame that follows and are remembered.
func TestAppearanceIsChosenInSettings(t *testing.T) {
	_, s := newFakeEngine(t)
	s.prefsFile = filepath.Join(t.TempDir(), "desktop.json")
	s.navigate(settingsPage)
	tt := ui.NewTester(s.view, 1180, 900)
	for _, step := range []string{"English", "简体中文"} {
		if err := tt.Click(step); err != nil {
			t.Fatal(err)
		}
	}
	if m := has(tt, "外观", "容器", "引擎运行中"); m != "" || s.prefs.Lang != "zh-Hans" {
		t.Fatalf("missing %q after switching to Chinese", m)
	}
	if err := tt.Click("浅色"); err != nil || s.prefs.Theme != "light" || s.dark {
		t.Fatalf("theme: %q dark=%v %v", s.prefs.Theme, s.dark, err)
	}
	if err := tt.Click("使用深色"); err != nil || s.prefs.Theme != "dark" {
		t.Fatalf("theme card: %q %v", s.prefs.Theme, err)
	}
	if err := tt.Click("紫色"); err != nil || s.prefs.Accent != "purple" {
		t.Fatalf("accent: %q %v", s.prefs.Accent, err)
	}
	// The log size comes first: at 125% the row below the fold is out of
	// the window, and the tester does not scroll.
	if err := tt.Click("大"); err != nil || s.prefs.LogSize != 15 {
		t.Fatalf("log text: %v %v", s.prefs.LogSize, err)
	}
	if err := tt.Click("125%"); err != nil || s.prefs.Scale != 1.25 {
		t.Fatalf("text size: %v %v", s.prefs.Scale, err)
	}
	if p := loadPrefs(s.prefsFile); p.Lang != "zh-Hans" || p.Theme != "dark" || p.Accent != "purple" || p.Scale != 1.25 || p.LogSize != 15 {
		t.Fatalf("saved: %+v", p)
	}
}

// Saving the VM's size posts what the sliders hold; the button stays off
// until something differs from what the engine stored.
func TestResourcesAreSavedFromSettings(t *testing.T) {
	f, s := newFakeEngine(t)
	s.navigate(settingsPage)
	s.settingsTab = 1
	tt := ui.NewTester(s.view, 1180, 760)
	if m := has(tt, "Linux VM", "Running now with 4 CPUs and up to 8 GB.", "Turn off engine"); m != "" {
		t.Fatalf("missing %q in %q", m, tt.Texts())
	}
	if err := tt.Click("Save"); err == nil && len(f.changed()) != 0 {
		t.Fatalf("an unchanged size was saved: %v", f.changed())
	}
	s.cpus, s.memory = 6, 12
	tt.Frame()
	if err := tt.Click("Save"); err != nil {
		t.Fatal(err)
	}
	if got := f.changed(); len(got) != 1 || got[0] != "POST /kiri/resources" || !strings.Contains(f.body(), `"cpus":6`) || !strings.Contains(f.body(), `"memory_mib":12288`) {
		t.Fatalf("requests: %v %s", got, f.body())
	}
}

// A VM running with a size other than the saved one offers a restart,
// which is a sleep followed by a wake.
func TestRestartAppliesSavedResources(t *testing.T) {
	f, s := newFakeEngine(t)
	s.navigate(settingsPage)
	s.settingsTab = 1
	tt := ui.NewTester(s.view, 1180, 760)
	if _, found := tt.Find("Restart VM to apply"); found {
		t.Fatal("a restart was offered with nothing pending")
	}
	s.res.RunningCPUs = 2
	tt.Frame()
	for _, step := range []string{"Restart VM to apply", "Restart"} {
		if err := tt.Click(step); err != nil {
			t.Fatal(err)
		}
	}
	if got := f.changed(); len(got) != 2 || got[0] != "POST /kiri/sleep" || got[1] != "POST /kiri/wake" {
		t.Fatalf("requests: %v", got)
	}
}

// The proxy pane shows what is in use and saves a custom proxy.
func TestProxyIsSavedFromSettings(t *testing.T) {
	f, s := newFakeEngine(t)
	s.navigate(settingsPage)
	s.settingsTab = 2
	tt := ui.NewTester(s.view, 1180, 760)
	if m := has(tt, "For pulling and pushing images. In use: http://127.0.0.1:7890, except localhost, *.internal."); m != "" {
		t.Fatalf("missing %q in %q", m, tt.Texts())
	}
	if _, found := tt.Find("HTTP proxy"); found {
		t.Fatal("custom fields shown for the system's proxy")
	}
	if err := tt.Click("Custom"); err != nil {
		t.Fatal(err)
	}
	if m := has(tt, "HTTP proxy", "HTTPS proxy", "Bypass for"); m != "" {
		t.Fatalf("missing %q after choosing a custom proxy", m)
	}
	s.proxyDraft.HTTP = "proxy.corp:3128"
	tt.Frame()
	if err := tt.Click("On"); err != nil {
		t.Fatal(err)
	}
	if err := tt.Click("Save"); err != nil {
		t.Fatal(err)
	}
	if got := f.changed(); len(got) != 1 || got[0] != "POST /kiri/proxy" ||
		!strings.Contains(f.body(), `"mode":"custom"`) || !strings.Contains(f.body(), `"http":"proxy.corp:3128"`) || !strings.Contains(f.body(), `"containers":true`) {
		t.Fatalf("requests: %v %s", got, f.body())
	}
}

// Changing the domain ports posts both; equal ports cannot be sent.
func TestDomainPortsAreChangedFromSettings(t *testing.T) {
	f, s := newFakeEngine(t)
	s.navigate(settingsPage)
	s.settingsTab = 4
	tt := ui.NewTester(s.view, 1180, 760)
	if m := has(tt, "Ports", "Change ports"); m != "" {
		t.Fatalf("missing %q in %q", m, tt.Texts())
	}
	s.portHTTP, s.portHTTPS = "8080", "8080"
	tt.Frame()
	if err := tt.Click("Change ports"); err == nil && len(f.changed()) != 0 {
		t.Fatalf("equal ports were sent: %v", f.changed())
	}
	s.portHTTPS = "8443"
	tt.Frame()
	if err := tt.Click("Change ports"); err != nil {
		t.Fatal(err)
	}
	if got := f.changed(); len(got) != 1 || got[0] != "POST /kiri/domains" || !strings.Contains(f.body(), `"http":8080`) || !strings.Contains(f.body(), `"https":8443`) {
		t.Fatalf("requests: %v %s", got, f.body())
	}
}

// fakeLauncher stands in for `kiri domains`: it keeps the two machine
// settings and records what it was asked to change.
type fakeLauncher struct {
	state nameState
	calls []string
	fail  error
}

func (l *fakeLauncher) run(_ context.Context, args ...string) ([]byte, error) {
	if len(args) == 1 {
		l.state.Suffix = map[bool]string{false: "kiri.localhost", true: "kiri.local"}[l.state.On]
		return json.Marshal(l.state)
	}
	l.calls = append(l.calls, strings.Join(args, " "))
	if l.fail != nil {
		return nil, l.fail
	}
	if args[1] == "local" {
		l.state.On = args[2] == "on"
	} else {
		l.state.HTTPS = args[2] == "on"
	}
	return nil, nil
}

// Local names and HTTPS are switched through the launcher, and the pane
// shows what the machine has afterwards. A closed password prompt changes
// nothing and is not reported as an error.
func TestNamesAndHTTPSAreSwitchedThroughTheLauncher(t *testing.T) {
	_, s := newFakeEngine(t)
	l := &fakeLauncher{}
	s.launcher = l.run
	s.navigate(settingsPage)
	s.settingsTab = 4
	tt := ui.NewTester(s.view, 1180, 760)
	if m := has(tt, "Off: containers answer at <name>.kiri.localhost, which needs no setup.", "Compose services answer at <service>.<project>.kiri.localhost. Changing this needs admin rights (an administrator password), since it adds or removes /etc/resolver/kiri.local."); m != "" {
		t.Fatalf("missing %q in %q", m, tt.Texts())
	}
	if err := tt.Click("kiri.local"); err != nil {
		t.Fatal(err)
	}
	if m := has(tt, "On: running containers answer at <name>.kiri.local."); m != "" || !s.names.On {
		t.Fatalf("missing %q after turning names on: %v", m, l.calls)
	}
	if err := tt.Click("On"); err != nil {
		t.Fatal(err)
	}
	if !s.names.HTTPS || strings.Join(l.calls, ", ") != "domains local on, domains https on" {
		t.Fatalf("calls: %v, state %+v", l.calls, s.names)
	}
	l.fail = errors.New("canceled")
	if err := tt.Click("Off"); err != nil {
		t.Fatal(err)
	}
	if !s.names.HTTPS || len(s.notices) != 0 {
		t.Fatalf("a closed prompt changed the state or was reported: %+v %v", s.names, s.notices)
	}
}

// Without a launcher there is nothing to switch: names are *.kiri.localhost.
func TestDomainsWithoutALauncherOfferNoSwitch(t *testing.T) {
	_, s := newFakeEngine(t)
	s.navigate(settingsPage)
	s.settingsTab = 4
	tt := ui.NewTester(s.view, 1180, 760)
	if m := has(tt, "Running containers answer at <name>.kiri.localhost, which needs no setup.", "Change ports"); m != "" {
		t.Fatalf("missing %q in %q", m, tt.Texts())
	}
	if _, found := tt.Find("kiri.local"); found {
		t.Fatal("a names switch is shown with no launcher to make it")
	}
}

// CPU is the container's share of the system's time between two samples,
// scaled to cores; the first sample alone says nothing.
func TestUsageAdvance(t *testing.T) {
	var u usage
	u.advance(sample{total: 100, system: 1000, cpus: 4, memory: 64})
	if u.cpu != 0 || len(u.hist) != 0 || u.memory != 64 {
		t.Fatalf("first sample: %+v", u)
	}
	u.advance(sample{total: 150, system: 2000, cpus: 4, memory: 80})
	if u.cpu != 20 || len(u.hist) != 1 || u.memory != 80 {
		t.Fatalf("second sample: %+v", u)
	}
}

func TestPrefsRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "desktop.json")
	if p := loadPrefs(path); p.language("zh-Hans-CN") != "zh-Hans" || p.language("fr") != "fr" || p.Theme != "system" || p.DockWidth != 400 {
		t.Fatalf("defaults: %+v", p)
	}
	prefs{Theme: "light", Lang: "en", Density: "cozy", NavCollapsed: true, DockWidth: 500}.save(path)
	if p := loadPrefs(path); p.Theme != "light" || p.Lang != "en" || !p.NavCollapsed || p.DockWidth != 500 {
		t.Fatalf("saved: %+v", p)
	}
}

// With KIRI_SHOTS set to a directory, this renders each page in both
// appearances there, for a look at the result without opening a window.
func TestScreenshots(t *testing.T) {
	dir := os.Getenv("KIRI_SHOTS")
	if dir == "" {
		t.Skip("set KIRI_SHOTS to a directory to render the pages")
	}
	shot := func(name string, s *desktop) {
		tt := ui.NewTester(s.view, 1180, 760)
		tt.Frame()
		file, err := os.Create(filepath.Join(dir, name+".png"))
		if err != nil {
			t.Fatal(err)
		}
		defer file.Close()
		if err := png.Encode(file, tt.Image()); err != nil {
			t.Fatal(err)
		}
	}
	for _, theme := range []string{"dark", "light"} {
		_, s := newFakeEngine(t)
		s.prefs.Theme, s.prefs.Lang = theme, "zh"
		s.launcher = (&fakeLauncher{state: nameState{On: true, HTTPS: true}}).run
		s.selectRow("c-api")
		shot("containers-"+theme, s)
		// A project's panel, and the bar of picked containers.
		s.selectRow(projectKey("shop"))
		s.picked["c-api"], s.picked["c-redis"] = true, true
		shot("containers-project-"+theme, s)
		s.tab[containersPage+"/project"] = 1
		shot("containers-project-config-"+theme, s)
		s.tab[containersPage+"/project"] = 0
		clear(s.picked)
		s.selectRow("c-api")
		// The detail panel's narrower tabs: inspect fields and the
		// container's own settings.
		s.filePath["c-api"] = "/app/"
		for _, tab := range []int{0, 3, 4} {
			s.tab[containersPage] = tab
			shot(fmt.Sprintf("containers-tab%d-%s", tab, theme), s)
		}
		s.tab[containersPage] = 1
		s.navigate(overviewPage)
		s.refresh()
		shot("overview-"+theme, s)
		s.navigate(imagesPage)
		s.selectRow("sha256:img-api")
		s.tab[imagesPage] = 1
		shot("images-"+theme, s)
		s.navigate(volumesPage)
		shot("volumes-"+theme, s)
		s.navigate(networksPage)
		shot("networks-"+theme, s)
		s.navigate(buildsPage)
		shot("builds-"+theme, s)
		s.navigate(settingsPage)
		for i, tab := range settingsTabs {
			s.settingsTab = i
			shot("settings-"+tab.id+"-"+theme, s)
		}
		s.navigate(containersPage)
		// A small window, with and without the detail panel.
		for _, sel := range []string{"", "c-api"} {
			s.selectRow(sel)
			// A list's state belongs to one view; this tester is another.
			s.lists = map[string]*ui.ListState{}
			tt := ui.NewTester(s.view, 900, 700)
			tt.Frame()
			file, err := os.Create(filepath.Join(dir, fmt.Sprintf("small%s-%s.png", strings.TrimPrefix(sel, "c"), theme)))
			if err != nil {
				t.Fatal(err)
			}
			if err := png.Encode(file, tt.Image()); err != nil {
				t.Fatal(err)
			}
			file.Close()
		}
		s.lists = map[string]*ui.ListState{}
		s.prefs.NavCollapsed = true
		s.selectRow("")
		shot("collapsed-"+theme, s)
	}
}

// With KIRI_FAKE_SOCKET set to a path, this serves the fake engine on that
// unix socket until the test is interrupted, so the real window can be
// started against it:
//
//	KIRI_FAKE_SOCKET=/tmp/kiri-fake/run/docker.sock go test -run TestServeFakeEngine -timeout 0 .
//	KIRI_SOCKET=/tmp/kiri-fake/run/docker.sock go tool mygo dev
func TestServeFakeEngine(t *testing.T) {
	socket := os.Getenv("KIRI_FAKE_SOCKET")
	if socket == "" {
		t.Skip("set KIRI_FAKE_SOCKET to a socket path to serve the fake engine")
	}
	f, _ := newFakeEngine(t)
	f.mu.Lock()
	f.shell = runtime.GOOS == "darwin"
	f.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(socket), 0o700); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(socket)
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("fake engine on %s", socket)
	_ = http.Serve(listener, f.server.Config.Handler)
}

// serveShell answers an interactive exec of the preview engine with a real
// shell on this Mac, run by containerShell inside script(1)'s
// pseudo-terminal, so that the Terminal tab can be tried by hand. A TTY
// exec's stream is raw, with no frame headers; the connection closes when
// the shell ends.
func (f *fakeEngine) serveShell(conn net.Conn, in io.Reader) {
	cmd := exec.Command("script", "-q", "/dev/null", "/bin/sh", "-c", containerShell)
	cmd.Dir, cmd.Env = os.Getenv("HOME"), append(os.Environ(), "TERM=xterm-256color", "BASH_SILENCE_DEPRECATION_WARNING=1")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = in, conn, conn
	if err := cmd.Start(); err != nil {
		conn.Close()
		return
	}
	f.mu.Lock()
	f.proc = cmd.Process
	f.mu.Unlock()
	go func() {
		_ = cmd.Wait()
		conn.Close()
	}()
}

// Everything that acts on a click shows the pointing hand; a button that is
// disabled, and plain content, keep the arrow.
func TestClickableElementsShowThePointingHand(t *testing.T) {
	_, s := newFakeEngine(t)
	s.navigate(imagesPage)
	tt := ui.NewTester(s.view, 1180, 760)
	over := func(label string) ui.Cursor {
		t.Helper()
		r, found := tt.Find(label)
		if !found {
			t.Fatalf("missing %q", label)
		}
		tt.Move(r.X+r.W/2, r.Y+r.H/2)
		return tt.Cursor()
	}
	for _, label := range []string{"Containers", "Pull image", "In use", "Details for alpine:3.21", "Delete alpine:3.21", "Hide sidebar"} {
		if got := over(label); got != ui.CursorPointer {
			t.Errorf("%q shows cursor %v", label, got)
		}
	}
	// nginx is in use, so its delete button is disabled.
	if got := over("Delete nginx:1.27-alpine"); got == ui.CursorPointer {
		t.Error("a disabled button shows the pointing hand")
	}
}
