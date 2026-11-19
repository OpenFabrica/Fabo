//go:build linux || darwin

package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// fakeMCPEngine serves the few reads fabo mcp makes on a short unix socket.
func fakeMCPEngine(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "fabo-mcp")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	socket := filepath.Join(dir, "docker.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	frame := func(stream byte, text string) []byte {
		header := []byte{stream, 0, 0, 0, 0, 0, 0, 0}
		binary.BigEndian.PutUint32(header[4:], uint32(len(text)))
		return append(header, text...)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("fabo mcp changed something: %s %s", r.Method, r.URL)
		}
		switch r.URL.Path {
		case "/info":
			w.Write([]byte(`{"ServerVersion":"27.5.1","FaboVersion":"0.0.0-dev","Containers":3,"ContainersRunning":2,"Images":3,"FaboVmAwake":true}`))
		case "/fabo/domains":
			w.Write([]byte(`{"http":{"port":80},"https":{"port":8443}}`))
		case "/containers/json":
			w.Write([]byte(`[
			 {"Id":"aaaaaaaaaaaaaaaa","Names":["/shop-web-1"],"Image":"nginx:1.27-alpine","ImageID":"sha256:nginxxxxxxxxxxxxxxx","State":"running","Status":"Up 1 hour",
			  "Labels":{"com.docker.compose.project":"shop","com.docker.compose.service":"web","com.docker.compose.project.working_dir":"/src/shop",
			   "com.docker.compose.project.config_files":"/src/shop/compose.yaml","com.docker.compose.version":"2.32.1"},
			  "Ports":[{"Type":"tcp","PrivatePort":80,"PublicPort":8080},{"Type":"tcp","PrivatePort":80,"PublicPort":8080}]},
			 {"Id":"bbbbbbbbbbbbbbbb","Names":["/shop-db-1"],"Image":"postgres:17-alpine","ImageID":"sha256:pg17alpinexxxxxxxxx","State":"running","Status":"Up 1 hour",
			  "Labels":{"com.docker.compose.project":"shop","com.docker.compose.service":"db"}},
			 {"Id":"cccccccccccccccc","Names":["/scratch"],"Image":"alpine","ImageID":"sha256:alpine","State":"exited","Status":"Exited (0)"}]`))
		case "/images/json":
			w.Write([]byte(`[
			 {"Id":"sha256:pg17alpinexxxxxxxxx","RepoTags":["index.docker.io/library/postgres:17-alpine"],"Size":120586240,"Created":1790000000},
			 {"Id":"sha256:pg17debianxxxxxxxxx","RepoTags":["postgres:17","postgres:latest"],"Size":167772160,"Created":1795000000},
			 {"Id":"sha256:nginxxxxxxxxxxxxxxx","RepoTags":["nginx:1.27-alpine"],"Size":50331648,"Created":1780000000}]`))
		case "/containers/shop-web-1/json":
			w.Write([]byte(`{"Config":{"Tty":false}}`))
		case "/containers/shop-web-1/logs":
			if r.URL.Query().Get("tail") != "50" {
				t.Errorf("tail = %q", r.URL.Query().Get("tail"))
			}
			w.Write(frame(1, "GET / 200\nWARN slow upstream\n"))
			w.Write(frame(2, "ERROR upstream timed out\nGET /health 200\n"))
		case "/fabo/images/scan":
			if r.URL.Query().Get("name") == "nginx:1.27-alpine" {
				w.Write([]byte(`{"scanned":1790000000,"packages":40,"vulns":[{"id":"CVE-1","severity":"High"}]}`))
				return
			}
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"message":"no scan"}`))
		default:
			t.Errorf("unexpected request %s", r.URL)
			w.WriteHeader(http.StatusNotFound)
		}
	})}
	t.Cleanup(func() { server.Close() })
	go server.Serve(listener)
	return socket
}

// mcpSession sends lines to a server and returns each answer by its id.
func mcpSession(t *testing.T, s *mcpServer, lines ...string) map[string]map[string]any {
	t.Helper()
	var out bytes.Buffer
	if err := s.serve(strings.NewReader(strings.Join(lines, "\n")+"\n"), &out); err != nil {
		t.Fatal(err)
	}
	answers := map[string]map[string]any{}
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		var answer map[string]any
		if err := json.Unmarshal([]byte(line), &answer); err != nil {
			t.Fatalf("%v: %s", err, line)
		}
		answers[string(must(json.Marshal(answer["id"])))] = answer
	}
	return answers
}

func must(raw []byte, err error) []byte {
	if err != nil {
		panic(err)
	}
	return raw
}

// result decodes a tools/call answer's structured content.
func result(t *testing.T, answer map[string]any) map[string]any {
	t.Helper()
	res, _ := answer["result"].(map[string]any)
	if res == nil || res["isError"] == true {
		t.Fatalf("tool failed: %v", answer)
	}
	return res["structuredContent"].(map[string]any)
}

// An agent connects, learns to reuse local images, lists the read-only
// tools, and reads containers, logs, projects, images and scans; nothing it
// does changes the engine.
func TestMCPServesReadOnlyTools(t *testing.T) {
	s := &mcpServer{client: newCommandClient(fakeMCPEngine(t)), names: func() (string, bool) { return "fabo.local", true }}
	call := func(id int, tool, args string) string {
		return `{"jsonrpc":"2.0","id":` + strconv.Itoa(id) + `,"method":"tools/call","params":{"name":"` + tool + `","arguments":` + args + `}}`
	}
	got := mcpSession(t, s,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		call(3, "list_containers", `{}`),
		call(4, "container_logs", `{"container":"shop-web-1","tail":50,"level":"error"}`),
		call(5, "compose_projects", `{"project":"shop"}`),
		call(6, "find_local_images", `{"name":"postgres"}`),
		call(7, "image_scan", `{"image":"nginx:1.27-alpine"}`),
		call(8, "image_scan", `{"image":"postgres:17"}`),
		call(9, "engine_info", `{}`),
		call(10, "remove_everything", `{}`),
		`{"jsonrpc":"2.0","id":11,"method":"resources/list"}`,
		`not json`,
	)
	if len(got) != 12 {
		t.Fatalf("%d answers, want 11 requests and a parse error: %v", len(got), got)
	}

	init := got["1"]["result"].(map[string]any)
	if init["protocolVersion"] != mcpProtocol || !strings.Contains(init["instructions"].(string), "find_local_images") {
		t.Fatalf("initialize: %v", init)
	}
	tools := got["2"]["result"].(map[string]any)["tools"].([]any)
	if len(tools) != 6 {
		t.Fatalf("%d tools", len(tools))
	}
	for _, tool := range tools {
		if tool.(map[string]any)["annotations"].(map[string]any)["readOnlyHint"] != true {
			t.Fatalf("not read-only: %v", tool)
		}
	}

	containers := result(t, got["3"])["containers"].([]any)
	web := containers[2].(map[string]any)
	if len(containers) != 3 || web["name"] != "shop-web-1" || web["local_url"] != "https://web.shop.fabo.local:8443" ||
		len(web["ports"].([]any)) != 1 || containers[0].(map[string]any)["local_url"] != nil {
		t.Fatalf("containers: %v", containers)
	}
	logs := result(t, got["4"])
	if lines := logs["lines"].([]any); len(lines) != 1 || lines[0] != "ERROR upstream timed out" {
		t.Fatalf("error lines: %v", logs)
	}
	shop := result(t, got["5"])["projects"].([]any)[0].(map[string]any)
	if shop["working_dir"] != "/src/shop" || shop["running"] != float64(2) || len(shop["services"].([]any)) != 2 || shop["compose_version"] != "2.32.1" {
		t.Fatalf("project: %v", shop)
	}
	repos := result(t, got["6"])["repositories"].([]any)
	postgres := repos[0].(map[string]any)["images"].([]any)
	newest, alpine := postgres[0].(map[string]any), postgres[1].(map[string]any)
	// A tag in Docker Hub's full spelling joins the familiar ones.
	if len(repos) != 1 || repos[0].(map[string]any)["repository"] != "postgres" || alpine["tags"].([]any)[0] != "postgres:17-alpine" ||
		len(postgres) != 2 || len(newest["tags"].([]any)) != 2 || len(newest["used_by"].([]any)) != 0 ||
		alpine["used_by"].([]any)[0] != "shop-db-1" || alpine["size_mb"] != 115.0 {
		t.Fatalf("postgres images: %v", repos)
	}
	if scan := result(t, got["7"]); scan["scanned"] != true {
		t.Fatalf("scan: %v", scan)
	}
	if scan := result(t, got["8"]); scan["scanned"] != false {
		t.Fatalf("not scanned: %v", scan)
	}
	if info := result(t, got["9"]); info["local_address_suffix"] != "fabo.local" || info["ServerVersion"] != "27.5.1" || info["FaboVersion"] != "0.0.0-dev" {
		t.Fatalf("info: %v", info)
	}
	if got["10"]["error"] == nil || got["11"]["error"].(map[string]any)["code"] != float64(-32601) || got["null"]["error"].(map[string]any)["code"] != float64(-32700) {
		t.Fatalf("errors: %v %v %v", got["10"], got["11"], got["null"])
	}
}

// An engine older than FaboVersion reported Fabo's release as ServerVersion;
// engine_info still names it as FaboVersion.
func TestMCPEngineInfoFromOlderEngine(t *testing.T) {
	s := &mcpServer{client: newCommandClient(fakeVersionEngine(t, `{"ServerVersion":"0.4.0","Containers":1}`)), names: func() (string, bool) { return "fabo.local", false }}
	got, err := s.engineInfo()
	if err != nil {
		t.Fatal(err)
	}
	if info := got.(map[string]any); info["FaboVersion"] != "0.4.0" || info["ServerVersion"] != "0.4.0" {
		t.Fatalf("info: %v", info)
	}
}
