//go:build darwin || linux

package main

import (
	"fmt"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"
)

// TestLiveKubernetesRefresh only reads the existing cluster. It never starts
// or stops the node or changes workloads.
func TestLiveKubernetesRefresh(t *testing.T) {
	socket := os.Getenv("FABO_LIVE_SOCKET")
	if socket == "" {
		t.Skip("set FABO_LIVE_SOCKET to test Kubernetes against a live engine")
	}
	t.Setenv("FABO_SOCKET", socket)
	s := newDesktop(newEngineClient(dialEngine))
	t.Cleanup(s.closeInteractive)
	s.prefs.Lang = "en"
	s.navigate(kubernetesPage)
	if s.kube.problem != "" || s.kube.state != "running" {
		t.Fatalf("live cluster unavailable: state=%s problem=%s", s.kube.state, s.kube.problem)
	}
	tt := ui.NewTester(s.view, 1180, 760)
	for _, tab := range []string{"Pods", "Services"} {
		click(t, tt, tab)
		for range 3 {
			s.refresh()
			tt.Frame()
			button, found := tt.Access("Refresh")
			if !found || button.Disabled || s.kube.problem != "" {
				t.Fatalf("%s refresh did not settle: button=%+v problem=%s", tab, button, s.kube.problem)
			}
		}
		click(t, tt, "Refresh")
		if s.kube.loading || s.kube.refreshing || s.kube.problem != "" {
			t.Fatalf("%s manual refresh did not settle: %+v", tab, s.kube)
		}
		t.Logf("%s refreshed successfully (%d cluster objects)", tab, len(s.kube.items))
		if dir := os.Getenv("FABO_SHOTS"); dir != "" {
			tt.Frame()
			file, err := os.Create(filepath.Join(dir, "live-kubernetes-"+strings.ToLower(tab)+".png"))
			if err != nil {
				t.Fatal(err)
			}
			err = png.Encode(file, tt.Image())
			file.Close()
			if err != nil {
				t.Fatal(err)
			}
		}
	}
}

// waitLive frames the window until condition holds, for a real engine's
// pace: images start, logs arrive and shells answer within seconds.
func waitLive(t *testing.T, tt *ui.Tester, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(50 * time.Millisecond)
		tt.Frame()
	}
}

// TestLiveEngine drives the window against a running engine, as a person
// would, and checks each part of the containers page on real containers:
//
//	FABO_LIVE_SOCKET=~/.fabo-x/run/docker.sock FABO_LIVE_PROJECT=demo \
//	  FABO_SHOTS=/tmp/shots go test -run TestLiveEngine -v -timeout 10m .
//
// It only reads unless FABO_LIVE_PROJECT names a Compose project it may
// stop, start and bring up again from its own, unchanged, Compose file.
func TestLiveEngine(t *testing.T) {
	socket := os.Getenv("FABO_LIVE_SOCKET")
	if socket == "" {
		t.Skip("set FABO_LIVE_SOCKET to an engine socket to test against it")
	}
	t.Setenv("FABO_SOCKET", socket)
	project, shots := os.Getenv("FABO_LIVE_PROJECT"), os.Getenv("FABO_SHOTS")
	s := newDesktop(newEngineClient(dialEngine))
	s.contextName = strings.TrimPrefix(filepath.Base(filepath.Dir(filepath.Dir(socket))), ".")
	s.prefs.Lang = "en"
	tt := ui.NewTester(s.view, 1280, 800)
	shot := func(name string) {
		if shots == "" {
			return
		}
		tt.Frame()
		file, err := os.Create(filepath.Join(shots, "live-"+name+".png"))
		if err != nil {
			t.Fatal(err)
		}
		defer file.Close()
		if err := png.Encode(file, tt.Image()); err != nil {
			t.Fatal(err)
		}
	}
	refresh := func() {
		s.refreshing = false
		s.refresh()
		tt.Frame()
	}
	refresh()
	waitLive(t, tt, "containers", func() bool { return len(s.containers) > 0 })
	// The list builds only the rows in view, so the window is made tall
	// enough to show every container.
	tt.SetSize(1280, min(8000, 400+70*len(s.containers)))
	tt.Frame()

	// The list: every container by name, a project as a heading, and a
	// brand tile for the images Fabo's catalog knows.
	var running container
	for _, x := range s.containers {
		if !tt.HasText(x.Name) {
			t.Errorf("container %s is not listed", x.Name)
		}
		if _, _, known := imageBrand(x.Image); known {
			t.Logf("%s shows the %s logo", x.Name, x.Image)
		}
		if x.State == "running" && (running.ID == "" || x.Project == project) {
			running = x
		}
	}
	if project != "" && !tt.HasText(project) {
		t.Errorf("project %s has no heading", project)
	}
	shot("containers")
	if running.ID == "" {
		t.Fatal("no running container to open")
	}
	t.Logf("opening %s (%s)", running.Name, running.Image)

	// Logs: the live stream fills the dark well.
	s.selectRow(running.ID)
	s.tab[containersPage] = 1
	waitLive(t, tt, "logs", func() bool { return len(s.visibleContainerLogs().lines) > 0 })
	shot("logs")

	// Files: a real folder, a text file's preview, and a copy in Downloads,
	// a temporary one while HOME points elsewhere.
	home := os.Getenv("HOME")
	t.Setenv("HOME", t.TempDir())
	s.tab[containersPage] = 3
	s.filePath[running.ID] = "/etc/"
	waitLive(t, tt, "the /etc listing", func() bool { return s.detail.files != nil || s.detail.filesErr != "" })
	if s.detail.filesErr != "" {
		t.Fatalf("listing /etc: %s", s.detail.filesErr)
	}
	t.Logf("/etc holds %d entries", len(s.detail.files))
	shot("files")
	s.openPreview(running, "/etc/os-release", 0)
	waitLive(t, tt, "the preview", func() bool { return s.detail.preview.loaded })
	if pv := s.detail.preview; pv.problem != "" || !strings.Contains(pv.text, "NAME=") {
		t.Fatalf("preview of /etc/os-release: %+v", pv)
	}
	shot("preview")
	s.downloadFile(running, "/etc/os-release")
	if data, err := os.ReadFile(filepath.Join(os.Getenv("HOME"), "Downloads", "os-release")); err != nil || !strings.Contains(string(data), "NAME=") {
		t.Fatalf("saved os-release: %q %v", data, err)
	}
	t.Setenv("HOME", home)

	// Terminal: a shell that answers what is typed.
	s.tab[containersPage] = 2
	tt.Frame()
	waitLive(t, tt, "the shell", func() bool {
		term, exited, problem, _ := s.terminals["container/"+running.ID].snapshot()
		if problem != "" || exited {
			t.Fatalf("terminal: exited %v, %q", exited, problem)
		}
		return term != nil
	})
	term, _, _, _ := s.terminals["container/"+running.ID].snapshot()
	term.Send([]byte("echo fabo-$((40+2))\r"))
	waitLive(t, tt, "the shell's answer", func() bool { return strings.Contains(term.Text(), "fabo-42") })
	t.Logf("terminal screen:\n%s", strings.TrimSpace(term.Text()))
	shot("terminal")
	s.retireTerminal("container/" + running.ID)

	if project == "" {
		return
	}
	var members []container
	for _, x := range s.containers {
		if x.Project == project {
			members = append(members, x)
		}
	}

	// The project's panel: what its labels say, and its Compose file.
	s.selectRow(projectKey(project))
	s.tab[containersPage+"/project"] = 0
	tt.Frame()
	if pr := readProject(project, members); len(pr.files) == 0 || pr.workDir == "" || pr.version == "" {
		t.Errorf("project facts: %+v", pr)
	}
	shot("project")
	s.tab[containersPage+"/project"] = 1
	waitLive(t, tt, "the Compose file", func() bool { return s.compose.loaded || s.compose.problem != "" })
	if s.compose.problem != "" {
		t.Fatalf("Compose file: %s", s.compose.problem)
	}
	shot("project-config")

	// Save and apply, with the file as it is: docker compose up -d runs
	// against the engine's context and leaves the project running.
	s.saveCompose(readProject(project, members), true)
	if s.compose.problem != "" {
		t.Fatalf("save and apply: %s", s.compose.problem)
	}

	// Picked together: the bar stops all of the project, then starts it.
	s.selectRow("")
	clear(s.picked)
	s.togglePicked(members)
	refresh()
	shot("picked")
	if err := tt.Click("Stop"); err != nil {
		t.Fatal(err)
	}
	waitLive(t, tt, "the project to stop", func() bool {
		refresh()
		for _, x := range s.containers {
			if x.Project == project && x.State == "running" {
				return false
			}
		}
		return true
	})
	if err := tt.Click("Start"); err != nil {
		t.Fatal(err)
	}
	waitLive(t, tt, "the project to start", func() bool {
		refresh()
		for _, x := range s.containers {
			if x.Project == project && x.State != "running" {
				return false
			}
		}
		return true
	})
	clear(s.picked)
	shot("restarted")
}

// TestLiveDebugContainerNetwork runs Debug sessions against a live engine:
// a TCP and a UDP client of a container's unpublished ports through the
// engine, and a UDP server here that a container sends to, named by the
// engine's relay. It needs FABO_LIVE_SOCKET and a running container
// FABO_LIVE_DEBUG_CONTAINER that answers "pong" on TCP 5432 and listens on
// UDP 8125, and a running container FABO_LIVE_DEBUG_SENDER with nc.
func TestLiveDebugContainerNetwork(t *testing.T) {
	socket := os.Getenv("FABO_LIVE_SOCKET")
	target, sender := os.Getenv("FABO_LIVE_DEBUG_CONTAINER"), os.Getenv("FABO_LIVE_DEBUG_SENDER")
	if socket == "" || target == "" || sender == "" {
		t.Skip("set FABO_LIVE_SOCKET, FABO_LIVE_DEBUG_CONTAINER and FABO_LIVE_DEBUG_SENDER")
	}
	t.Setenv("FABO_SOCKET", socket)
	s := newDesktop(newEngineClient(dialEngine))
	s.prefs.Lang = "en"
	s.prefsFile = filepath.Join(t.TempDir(), "desktop.json")
	s.refresh()
	s.navigate(debugPage)
	t.Cleanup(func() {
		for _, r := range s.debug.runs {
			r.stop()
		}
	})
	tcp := debugSession{ID: "tcp", Kind: debugClient, Proto: debugTCP, Name: "db", Container: target, Port: 5432, Via: "net"}
	s.debugState().sessions = []debugSession{tcp}
	s.startDebug(tcp)
	waitFor(t, s.debugRunOf("tcp"), "pong", received("pong\n"))

	udp := debugSession{ID: "udp", Kind: debugClient, Proto: debugUDP, Name: "statsd", Container: target, Port: 8125, Via: "net"}
	s.startDebug(udp)
	r := s.debugRunOf("udp")
	waitFor(t, r, "open", func(x debugSnap) bool { return x.state == debugOpen })
	if err := r.sendClient([]byte("live:1|c"), false); err != nil {
		t.Fatal(err)
	}

	port := freePort(t, "udp")
	server := debugSession{ID: "srv", Kind: debugServer, Proto: debugUDP, Name: "logs", Port: port, AutoReply: true, Reply: "ACK"}
	s.debug.sessions = append(s.debug.sessions, server)
	s.debug.cur = server.ID
	s.startDebug(server)
	sr := s.debugRunOf("srv")
	waitFor(t, sr, "listening", func(x debugSnap) bool { return x.state == debugListening })
	out, err := exec.Command("docker", "exec", sender, "sh", "-c", fmt.Sprintf("echo -n hi | nc -u -w 2 host.docker.internal %d", port)).CombinedOutput()
	if err != nil || string(out) != "ACK" {
		t.Fatalf("container got %q, %v", out, err)
	}
	tt := ui.NewTester(s.view, 1180, 760)
	deadline := time.Now().Add(5 * time.Second)
	for !tt.HasText(sender) && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
		tt.Advance(200 * time.Millisecond)
		tt.Frame()
	}
	if !tt.HasText(sender) {
		t.Fatalf("the server does not name its client %s", sender)
	}
}
