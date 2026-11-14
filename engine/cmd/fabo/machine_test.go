//go:build linux || darwin

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestMachineOptions(t *testing.T) {
	for _, args := range [][]string{nil, {"wat"}, {"create"}, {"delete", "../x"}, {"create", "-x"}, {"create", "bad_name"}, {"list", "x"}, {"create", "dev", "--distro"}, {"create", "dev", "--image", ""}, {"create", "dev", "--distro", "alpine", "--image", "ubuntu"}, {"stop", "dev", "--force"}, {"shell", "dev", "pwd"}} {
		if _, err := parseMachine(args); err == nil {
			t.Errorf("accepted %q", args)
		}
	}
	for distro, image := range machineDistros {
		op, err := parseMachine([]string{"create", "dev-1", "--distro", distro, "--systemd"})
		if err != nil || op.image != image || !op.systemd {
			t.Fatalf("%s: %+v, %v", distro, op, err)
		}
	}
	for alias, verb := range map[string]string{"ls": "list", "rm": "delete"} {
		args := []string{alias}
		if alias == "rm" {
			args = append(args, "dev")
		}
		op, err := parseMachine(args)
		if err != nil || op.verb != verb {
			t.Fatalf("alias %s: %+v, %v", alias, op, err)
		}
	}
	for _, ref := range []string{"alpine", "registry:5000/team/image", "registry:5000/team/image:tag", "team/image@sha256:abc"} {
		image, query := machineImage(ref)
		if strings.Contains(ref, "@") {
			if image != ref || query.Get("tag") != "" || query.Get("fromImage") != ref {
				t.Fatalf("digest changed: %s, %v", image, query)
			}
		} else if query.Get("fromImage")+":"+query.Get("tag") != image || !strings.HasPrefix(image, ref) {
			t.Fatalf("bad reference: %s, %v", image, query)
		}
	}
}

func machineTestClient(t *testing.T, handler http.HandlerFunc) *commandClient {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "fabo-machine-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	socket := filepath.Join(dir, "docker.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: handler}
	go server.Serve(listener)
	t.Cleanup(func() { server.Close() })
	client := newCommandClient(socket)
	t.Cleanup(client.http.CloseIdleConnections)
	return client
}

func TestMachineCreate(t *testing.T) {
	for _, scenario := range []string{"normal", "systemd", "pull-error", "pull-malformed", "create-error", "start-error", "user-error", "exec-no-status"} {
		t.Run(scenario, func(t *testing.T) {
			var calls []string
			client := machineTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				calls = append(calls, r.Method+" "+r.URL.Path)
				switch r.URL.Path {
				case "/images/create":
					if r.URL.Query().Get("fromImage") != "alpine" || r.URL.Query().Get("tag") != "3.22" {
						t.Errorf("pull query: %s", r.URL.RawQuery)
					}
					if scenario == "pull-error" {
						fmt.Fprintln(w, `{"status":"pulling"}`)
						fmt.Fprintln(w, `{"error":"registry denied"}`)
					} else if scenario == "pull-malformed" {
						fmt.Fprint(w, `{"status":`)
					} else {
						fmt.Fprintln(w, `{"status":"pulled"}`)
					}
				case "/containers/create":
					if scenario == "create-error" {
						w.WriteHeader(409)
						fmt.Fprint(w, `{"message":"name already exists"}`)
						return
					}
					var spec struct {
						Image, Hostname, StopSignal string
						Labels                      map[string]string
						Cmd, Env                    []string
						HostConfig                  struct {
							Binds, CapAdd []string
							RestartPolicy struct{ Name string }
							Privileged    bool
							Tmpfs         map[string]string
						}
					}
					if err := json.NewDecoder(r.Body).Decode(&spec); err != nil {
						t.Error(err)
					}
					if r.URL.Query().Get("name") != "machine-dev" || spec.Image != "alpine:3.22" || spec.Hostname != "dev" || spec.Labels[machineLabel] != "dev" || spec.HostConfig.RestartPolicy.Name != "unless-stopped" || !reflect.DeepEqual(spec.HostConfig.Binds, []string{machineHomes() + ":" + machineHomes()}) {
						t.Errorf("bad machine spec: %+v", spec)
					}
					if len(spec.Cmd) != 3 || !strings.Contains(spec.Cmd[2], "-u 501") || !strings.Contains(spec.Cmd[2], "-g 20") {
						t.Errorf("missing host identity: %v", spec.Cmd)
					}
					if scenario == "systemd" {
						if !spec.HostConfig.Privileged || spec.StopSignal != "SIGRTMIN+3" || len(spec.HostConfig.Tmpfs) != 3 || !reflect.DeepEqual(spec.Env, []string{"container=docker"}) {
							t.Errorf("systemd config: %+v", spec)
						}
					} else if spec.HostConfig.Privileged || spec.StopSignal != "" {
						t.Error("plain machine should not be privileged")
					}
					w.WriteHeader(201)
					fmt.Fprint(w, `{"Id":"container-id"}`)
				case "/containers/machine-dev/start":
					if scenario == "start-error" {
						w.WriteHeader(500)
						fmt.Fprint(w, `{"message":"guest failed"}`)
					} else {
						w.WriteHeader(204)
					}
				case "/containers/machine-dev/exec":
					var spec struct {
						Cmd                             []string
						Tty, AttachStdout, AttachStderr bool
					}
					if err := json.NewDecoder(r.Body).Decode(&spec); err != nil || !spec.Tty || !spec.AttachStdout || !spec.AttachStderr || len(spec.Cmd) != 3 || !strings.Contains(spec.Cmd[2], "id -u 'tester'") {
						t.Errorf("user readiness probe: %+v, %v", spec, err)
					}
					fmt.Fprint(w, `{"Id":"exec-id"}`)
				case "/exec/exec-id/start":
					conn, rw, err := http.NewResponseController(w).Hijack()
					if err != nil {
						t.Error(err)
						return
					}
					defer conn.Close()
					fmt.Fprint(rw, "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\nready\r\n")
					rw.Flush()
				case "/exec/exec-id/json":
					switch scenario {
					case "user-error":
						fmt.Fprint(w, `{"ExitCode":1,"Running":false}`)
					case "exec-no-status":
						fmt.Fprint(w, `{}`)
					default:
						fmt.Fprint(w, `{"ExitCode":0,"Running":false}`)
					}
				default:
					t.Errorf("unexpected request: %s %s", r.Method, r.URL)
					w.WriteHeader(404)
				}
			})
			var output bytes.Buffer
			op := machineOptions{verb: "create", name: "dev", image: "alpine:3.22", systemd: scenario == "systemd"}
			err := runMachine(client, op, machineUser{"tester", 501, 20}, &output)
			wantError := map[string]string{"pull-error": "registry denied", "pull-malformed": "invalid pull response", "create-error": "name already exists", "start-error": "guest failed", "user-error": "could not be added", "exec-no-status": "without an exit status"}[scenario]
			if wantError != "" {
				if err == nil || !strings.Contains(err.Error(), wantError) || strings.Contains(output.String(), "is running:") {
					t.Fatalf("err=%v output=%s", err, &output)
				}
			} else if err != nil || !strings.Contains(output.String(), "is running: fabo machine shell dev") {
				t.Fatalf("err=%v output=%s", err, &output)
			}
			if strings.HasPrefix(scenario, "pull-") && len(calls) != 1 || scenario == "create-error" && len(calls) != 2 || scenario == "start-error" && len(calls) != 3 {
				t.Fatalf("continued after failure: %v", calls)
			}
		})
	}
}

func TestMachineLifecycle(t *testing.T) {
	for _, verb := range []string{"list", "start", "stop", "restart", "delete"} {
		for _, status := range []int{200, 304, 404, 500} {
			t.Run(fmt.Sprintf("%s-%d", verb, status), func(t *testing.T) {
				client := machineTestClient(t, func(w http.ResponseWriter, r *http.Request) {
					method, path := "POST", "/containers/machine-dev/"+verb
					if verb == "list" {
						method, path = "GET", "/containers/json"
						if r.URL.Query().Get("all") != "1" || r.URL.Query().Get("filters") != `{"label":["dev.fabo.machine"]}` {
							t.Error("missing machine filter")
						}
					}
					if verb == "delete" {
						method, path = "DELETE", "/containers/machine-dev"
						if r.URL.Query().Get("force") != "1" || r.URL.Query().Get("v") != "1" {
							t.Error("delete must remove anonymous volumes")
						}
					}
					if r.Method != method || r.URL.Path != path {
						t.Errorf("unexpected request %s %s", r.Method, r.URL)
					}
					w.WriteHeader(status)
					if status == 200 && verb == "list" {
						fmt.Fprint(w, `[{"Labels":{"dev.fabo.machine":"dev"},"Image":"alpine:3.22","Status":"Up 1 minute"}]`)
					} else if status >= 400 {
						fmt.Fprint(w, `{"message":"engine rejected action"}`)
					}
				})
				var out bytes.Buffer
				err := runMachine(client, machineOptions{verb: verb, name: "dev"}, machineUser{}, &out)
				success := status == 200 || status == 304 && (verb == "start" || verb == "stop")
				if (err == nil) != success {
					t.Fatalf("error=%v output=%s", err, &out)
				}
				if status >= 400 && (err.Error() != "engine rejected action" || strings.Contains(out.String(), "deleted")) {
					t.Fatalf("error=%v output=%s", err, &out)
				}
				if verb == "list" && status == 200 && (!strings.Contains(out.String(), "dev") || !strings.Contains(out.String(), "alpine:3.22") || !strings.Contains(out.String(), "Up 1 minute")) {
					t.Fatalf("list=%s", &out)
				}
			})
		}
	}
}

func TestMachineShellArguments(t *testing.T) {
	user := machineUser{"tester", 501, 20}
	for _, cwd := range []string{machineHomes() + "/tester/project", machineHomes() + "-other/project", "/tmp", ""} {
		for _, tty := range []bool{false, true} {
			op := machineOptions{name: "dev", command: []string{"printf", "%s", "a b", "*"}}
			args := machineShellArgs(op, user, cwd, tty)
			wantDir := user.home()
			if cwd == machineHomes()+"/tester/project" {
				wantDir = cwd
			}
			wantTTY := "-i"
			if tty {
				wantTTY = "-it"
			}
			want := []string{"exec", wantTTY, "-u", "tester", "-w", wantDir, "-e", "HOME=" + user.home(), "machine-dev", "printf", "%s", "a b", "*"}
			if !reflect.DeepEqual(args, want) {
				t.Fatalf("args=%q want=%q", args, want)
			}
		}
	}
	args := machineShellArgs(machineOptions{name: "dev"}, machineUser{"root", 0, 0}, "/tmp", false)
	if args[5] != "/root" || args[7] != "HOME=/root" || args[len(args)-1] != "exec $(command -v bash || command -v sh) -l" {
		t.Fatalf("root shell: %q", args)
	}
}

func TestMachineShellProcess(t *testing.T) {
	if os.Getenv("FABO_TEST_MACHINE") == "1" {
		if err := machineCommand([]string{"shell", "dev", "--", "echo", "a b", "*"}); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(99)
	}
	client := machineTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/_ping" {
			t.Errorf("unexpected %s", r.URL)
		}
		fmt.Fprint(w, "OK")
	})
	dir := t.TempDir()
	script := "#!/bin/sh\nprintf '%s\\n' \"$DOCKER_HOST\" \"${DOCKER_CONTEXT-unset}\" \"$@\"\n/bin/cat\nprintf 'shell stderr\\n' >&2\nexit 37\n"
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	t.Setenv("FABO_SOCKET", client.socket)
	t.Setenv("DOCKER_CONTEXT", "unrelated")
	t.Setenv("FABO_TEST_MACHINE", "1")
	cmd := exec.Command(os.Args[0], "-test.run=^TestMachineShellProcess$")
	cmd.Dir = dir
	cmd.Stdin = strings.NewReader("shell stdin\n")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 37 {
		t.Fatalf("exit=%v stderr=%s", err, &stderr)
	}
	want := "unix://" + client.socket + "\nunset\n" + strings.Join(machineShellArgs(machineOptions{name: "dev", command: []string{"echo", "a b", "*"}}, hostMachineUser(), dir, false), "\n") + "\nshell stdin\n"
	if stdout.String() != want || stderr.String() != "shell stderr\n" {
		t.Fatalf("stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

func TestMachineInitQuotesEnvironment(t *testing.T) {
	user := machineUser{"user'$(exit 99)", 501, 20}
	script := machineInit(user, false)
	// Run just the assignments: expansion must leave the username literal.
	prefix := strings.SplitN(script, "if ! id", 2)[0] + "printf '%s' \"$u\""
	output, err := exec.Command("/bin/sh", "-c", prefix).CombinedOutput()
	if err != nil || string(output) != user.name {
		t.Fatalf("user=%q err=%v", output, err)
	}
	for _, systemd := range []bool{false, true} {
		cmd := exec.Command("/bin/sh", "-n")
		cmd.Stdin = strings.NewReader(machineInit(user, systemd))
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("shell syntax: %v %s", err, out)
		}
	}
}

func TestCommandTimeoutAndInvalidResponse(t *testing.T) {
	client := machineTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/slow" {
			<-r.Context().Done()
			return
		}
		fmt.Fprint(w, `{"broken":`)
	})
	client.http.Timeout = 30 * time.Millisecond
	if err := client.request("GET", "/slow", nil, nil); err == nil {
		t.Fatal("missing timeout")
	}
	var out map[string]any
	if err := client.request("GET", "/invalid", nil, &out); err == nil {
		t.Fatal("accepted invalid JSON")
	}
	if err := runMachine(client, machineOptions{verb: "create", name: "dev", image: "alpine"}, machineUser{"../bad", 501, 20}, io.Discard); err == nil || !strings.Contains(err.Error(), "invalid host user") {
		t.Fatalf("user validation: %v", err)
	}
}
