//go:build linux || darwin

package main

import (
	"bytes"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"testing"
)

func TestDockerCommands(t *testing.T) {
	for _, command := range []string{"run", "exec", "compose", "buildx", "container", "version", "wait"} {
		if !handlesDocker(command) {
			t.Errorf("missing command %s", command)
		}
	}
	for _, command := range []string{"import", "machine", "setup", "daemon", "nonsense", "--host"} {
		if handlesDocker(command) {
			t.Errorf("reserved/unknown command %s", command)
		}
	}
	if handlesDocker("update") != (runtime.GOOS == "linux") {
		t.Fatal("update dispatch differs from upstream")
	}
}

func unsetEnv(t *testing.T, key string) {
	t.Helper()
	t.Setenv(key, "")
	if err := os.Unsetenv(key); err != nil {
		t.Fatal(err)
	}
}

func TestDockerEnvironment(t *testing.T) {
	in := installation{home: t.TempDir(), names: namesFor("dev.kiri.app.dev")}
	t.Setenv("KIRI_SOCKET", "/private/test engine.sock")
	t.Setenv("DOCKER_HOST", "tcp://unrelated:2375")
	t.Setenv("DOCKER_CONTEXT", "unrelated")
	unsetEnv(t, "BUILDX_BUILDER")
	unsetEnv(t, "BUILDX_CONFIG")
	unsetEnv(t, "DOCKER_CONFIG")
	for _, source := range []string{"home", "docker", "buildx"} {
		t.Run(source, func(t *testing.T) {
			dir := filepath.Join(in.home, ".docker", "buildx")
			if source == "docker" {
				config := t.TempDir()
				t.Setenv("DOCKER_CONFIG", config)
				dir = filepath.Join(config, "buildx")
			}
			if source == "buildx" {
				dir = t.TempDir()
				t.Setenv("BUILDX_CONFIG", dir)
				t.Setenv("DOCKER_CONFIG", t.TempDir())
			}
			if slices.Contains(in.dockerEnvironment(), "BUILDX_BUILDER="+in.names.builder) {
				t.Fatal("selected missing builder")
			}
			path := filepath.Join(dir, "instances", in.names.builder)
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, nil, 0600); err != nil {
				t.Fatal(err)
			}
			env := in.dockerEnvironment()
			if !slices.Contains(env, "DOCKER_HOST=unix:///private/test engine.sock") || !slices.Contains(env, "BUILDX_BUILDER=kiri-dev-build") {
				t.Fatalf("endpoint/builder missing: %v", env)
			}
			for _, value := range env {
				if strings.HasPrefix(value, "DOCKER_CONTEXT=") || value == "DOCKER_HOST=tcp://unrelated:2375" {
					t.Fatalf("inherited wrong endpoint %s", value)
				}
			}
			for _, value := range []string{"custom", ""} {
				t.Setenv("BUILDX_BUILDER", value)
				if !slices.Contains(in.dockerEnvironment(), "BUILDX_BUILDER="+value) || slices.Contains(in.dockerEnvironment(), "BUILDX_BUILDER=kiri-dev-build") {
					t.Fatal("overrode explicit builder")
				}
			}
		})
	}
}

func TestDockerCLIResolution(t *testing.T) {
	root := t.TempDir()
	in := installation{dir: filepath.Join(root, "Contents", "MacOS")}
	t.Setenv("PATH", t.TempDir())
	if in.dockerCLI() != "" {
		t.Fatal("unexpected CLI")
	}
	bundled := filepath.Join(root, "Contents", "Resources", "bin", "docker")
	if err := os.MkdirAll(filepath.Dir(bundled), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bundled, []byte("#!/bin/sh\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if in.dockerCLI() != bundled {
		t.Fatal("bundle fallback missing")
	}
	onPath := filepath.Join(os.Getenv("PATH"), "docker")
	if err := os.WriteFile(onPath, []byte("#!/bin/sh\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if in.dockerCLI() != onPath {
		t.Fatal("PATH should win")
	}
}

// Exercise exec in a subprocess: replacing the test runner would hide failures.
func TestDockerExec(t *testing.T) {
	if os.Getenv("KIRI_TEST_DOCKER_EXEC") == "1" {
		in := installation{home: os.Getenv("HOME"), names: namesFor("")}
		if err := in.runDocker([]string{"run", "--rm", "image", "argument with spaces", "*"}); err != nil {
			os.Stderr.WriteString(err.Error())
			os.Exit(1)
		}
		os.Exit(99)
	}
	dir := t.TempDir()
	t.Setenv("PATH", dir)
	t.Setenv("DOCKER_CONTEXT", "wrong")
	t.Setenv("KIRI_TEST_DOCKER_EXEC", "1")
	// Keep Unix socket paths below macOS's length limit.
	socketDir, err := os.MkdirTemp("/tmp", "kiri-cli-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(socketDir)
	socket := filepath.Join(socketDir, "docker.sock")
	t.Setenv("KIRI_SOCKET", socket)
	cli := filepath.Join(dir, "docker")
	script := "#!/bin/sh\nprintf '%s\\n' \"$DOCKER_HOST\" \"${DOCKER_CONTEXT-unset}\" \"$@\"\n/bin/cat\nprintf 'docker stderr\\n' >&2\nexit 37\n"
	if err := os.WriteFile(cli, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/_ping" || r.Header.Get("X-Kiri-Background") != "1" {
			t.Errorf("unexpected probe: %s", r.URL.Path)
			w.WriteHeader(400)
			return
		}
		w.Write([]byte("OK"))
	})}
	defer server.Close()
	go server.Serve(listener)
	cmd := exec.Command(os.Args[0], "-test.run=^TestDockerExec$")
	cmd.Stdin = strings.NewReader("stdin survives\n")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err = cmd.Run()
	if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 37 {
		t.Fatalf("exit=%v stderr=%s", err, stderr.String())
	}
	want := "unix://" + socket + "\nunset\nrun\n--rm\nimage\nargument with spaces\n*\nstdin survives\n"
	if stdout.String() != want || stderr.String() != "docker stderr\n" {
		t.Fatalf("stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	if err := os.WriteFile(cli, []byte("#!/bin/sh\nkill -TERM $$\n"), 0700); err != nil {
		t.Fatal(err)
	}
	cmd = exec.Command(os.Args[0], "-test.run=^TestDockerExec$")
	err = cmd.Run()
	if exit, ok := err.(*exec.ExitError); !ok {
		t.Fatalf("signal exit = %v", err)
	} else if status, ok := exit.Sys().(syscall.WaitStatus); !ok || !status.Signaled() || status.Signal() != syscall.SIGTERM {
		t.Fatalf("Docker signal was not preserved: %v", exit.Sys())
	}
	server.Close()
	cmd = exec.Command(os.Args[0], "-test.run=^TestDockerExec$")
	output, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(output), "Kiri isn't running") {
		t.Fatalf("stopped engine: %s, %v", output, err)
	}
	if err := os.Remove(cli); err != nil {
		t.Fatal(err)
	}
	cmd = exec.Command(os.Args[0], "-test.run=^TestDockerExec$")
	output, err = cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(output), "no docker CLI found") {
		t.Fatalf("missing CLI: %s, %v", output, err)
	}
}
