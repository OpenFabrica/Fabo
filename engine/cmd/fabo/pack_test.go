//go:build linux || darwin

package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
)

// Volumes travel as name:destination binds rebuilt from Mounts, host binds
// stay, and a shared network names its container instead of its ID.
func TestPackSpec(t *testing.T) {
	var info map[string]any
	if err := json.Unmarshal([]byte(`{
	 "Id":"aaaa1111","Name":"/shop-web-1",
	 "Config":{"Image":"nginx","Env":["A=1"],"Labels":{"com.docker.compose.project":"shop"}},
	 "HostConfig":{"Binds":["/src/shop:/app","shop_data:/data"],"NetworkMode":"container:bbbb2222",
	  "Mounts":[{"Type":"volume","Source":"shop_cache","Target":"/cache"},{"Type":"tmpfs","Target":"/run"}]},
	 "Mounts":[{"Type":"bind","Source":"/src/shop","Destination":"/app","RW":true},
	  {"Type":"volume","Name":"shop_data","Destination":"/data","RW":true},
	  {"Type":"volume","Name":"shop_cache","Destination":"/cache","RW":false},
	  {"Type":"volume","Name":"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef","Destination":"/var/lib/x","RW":true}]}`), &info); err != nil {
		t.Fatal(err)
	}
	spec, volumes, err := packSpec(info, "fabo-pack/shop-web-1:1", map[string]string{"aaaa1111": "shop-web-1", "bbbb2222cccc": "shop-net-1"})
	if err != nil {
		t.Fatal(err)
	}
	host := spec["HostConfig"].(map[string]any)
	binds := host["Binds"].([]any)
	want := []any{"/src/shop:/app", "shop_data:/data", "shop_cache:/cache:ro", "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef:/var/lib/x"}
	if spec["Image"] != "fabo-pack/shop-web-1:1" || !slices.Equal(binds, want) || len(host["Mounts"].([]any)) != 1 ||
		host["NetworkMode"] != "container:shop-net-1" || len(volumes) != 3 {
		t.Fatalf("spec %v volumes %v", spec, volumes)
	}
	if _, _, err := packSpec(info, "x", map[string]string{"aaaa1111": "shop-web-1"}); err == nil || !strings.Contains(err.Error(), "pack that container too") {
		t.Fatalf("lone shared network: %v", err)
	}
	if packRepository("WeKnora_postgres--1") != "fabo-pack/weknora-postgres-1" || packRepository("__") != "fabo-pack/container" {
		t.Fatal(packRepository("WeKnora_postgres--1"))
	}
}

// Unpacking renames volume binds to the volumes made here and drops host
// paths this machine lacks.
func TestUnpackBinds(t *testing.T) {
	dir := t.TempDir()
	spec := map[string]any{"HostConfig": map[string]any{
		"Binds":  []any{dir + ":/app", "/no/such/fabo/dir:/gone", "anon:/data:ro"},
		"Mounts": []any{map[string]any{"Type": "bind", "Source": "/no/such/fabo/file"}, map[string]any{"Type": "tmpfs", "Target": "/run"}},
	}}
	missing := unpackBinds(spec, func(n string) string { return map[string]string{"anon": "fresh"}[n] })
	host := spec["HostConfig"].(map[string]any)
	if !slices.Equal(host["Binds"].([]any), []any{dir + ":/app", "fresh:/data:ro"}) || len(host["Mounts"].([]any)) != 1 ||
		!slices.Equal(missing, []string{"/no/such/fabo/dir", "/no/such/fabo/file"}) {
		t.Fatalf("binds %v missing %v", host, missing)
	}
}

// writeTestPackage writes a package of the given manifest and entries.
func writeTestPackage(t *testing.T, m packManifest, entries ...string) string {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	raw, _ := json.Marshal(m)
	add := func(name string, data []byte) {
		if err := writePackEntry(tw, name, bytes.NewReader(data), int64(len(data))); err != nil {
			t.Fatal(err)
		}
	}
	add("manifest.json", raw)
	for _, name := range entries {
		add(name, []byte("tar bytes of "+name))
	}
	tw.Close()
	zw.Close()
	path := filepath.Join(t.TempDir(), "test"+packExtension)
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// When a container cannot be created, unpack removes the containers,
// volumes and networks it made, and touches nothing that was already there.
func TestUnpackRollsBack(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "fabo-pack")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	socket := filepath.Join(dir, "docker.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var calls []string
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		calls = append(calls, r.Method+" "+r.URL.Path+"?"+r.URL.RawQuery)
		mu.Unlock()
		switch {
		case r.URL.Path == "/containers/json":
			w.Write([]byte(`[{"Names":["/other"]}]`))
		case r.URL.Path == "/volumes" && r.Method == "GET":
			w.Write([]byte(`{"Volumes":[{"Name":"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"}]}`))
		case r.URL.Path == "/networks" && r.Method == "GET":
			w.Write([]byte(`[{"Name":"bridge"},{"Name":"shared"}]`))
		case r.URL.Path == "/images/load":
			if string(body) != "tar bytes of images.tar" {
				t.Errorf("load body %q", body)
			}
			w.Write([]byte(`{"stream":"Loaded image: fabo-pack/a:1\n"}`))
		case r.URL.Path == "/containers/create" && r.URL.Query().Get("name") == "b":
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`{"message":"bad spec"}`))
		case r.Method == "POST" || r.Method == "DELETE":
			w.WriteHeader(http.StatusCreated)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL)
		}
	})}
	t.Cleanup(func() { server.Close() })
	go server.Serve(listener)

	anon := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	m := packManifest{Format: packFormat, Version: packVersion,
		Containers: []packContainer{
			{Name: "a", Spec: map[string]any{"Image": "fabo-pack/a:1", "HostConfig": map[string]any{"Binds": []any{anon + ":/data"}}}},
			{Name: "b", Spec: map[string]any{"Image": "fabo-pack/b:1", "HostConfig": map[string]any{}}},
		},
		Volumes:  []packVolume{{Name: "app_data", Driver: "local", Data: "volumes/0.tar"}, {Name: anon, Driver: "local"}},
		Networks: []packNetwork{{Name: "app_net", Driver: "bridge"}, {Name: "shared", Driver: "bridge"}},
	}
	file := writeTestPackage(t, m, "images.tar", "volumes/0.tar")
	err = unpackFile(newCommandClient(socket), file, false, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "bad spec") {
		t.Fatalf("unpack: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	joined := strings.Join(calls, "\n")
	var renamed string
	for _, call := range calls {
		if strings.HasPrefix(call, "DELETE /volumes/") && !strings.Contains(call, "app_data") {
			renamed = strings.TrimSuffix(strings.TrimPrefix(call, "DELETE /volumes/"), "?")
		}
	}
	for _, want := range []string{"POST /networks/create", "DELETE /containers/a?force=1", "DELETE /volumes/app_data?", "DELETE /networks/app_net?"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in\n%s", want, joined)
		}
	}
	// The anonymous volume already here got a fresh name, and only that
	// copy was removed; the shared network that existed was left alone.
	if renamed == "" || renamed == anon || strings.Contains(joined, "DELETE /networks/shared") || strings.Contains(joined, "DELETE /volumes/"+anon) {
		t.Errorf("calls:\n%s", joined)
	}

	// A name already taken stops the unpack before anything is made.
	calls = nil
	m.Containers[0].Name = "other"
	mu.Unlock()
	err = unpackFile(newCommandClient(socket), writeTestPackage(t, m, "images.tar"), false, io.Discard)
	mu.Lock()
	if err == nil || !strings.Contains(err.Error(), "container other") || slices.ContainsFunc(calls, func(c string) bool { return !strings.HasPrefix(c, "GET ") }) {
		t.Fatalf("taken name: %v %v", err, calls)
	}
}
