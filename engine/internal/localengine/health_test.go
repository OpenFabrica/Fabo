//go:build linux || darwin

package localengine

import (
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"testing/synctest"
	"time"

	"github.com/OpenFabrica/Fabo/engine/internal/guestwire"
)

func TestHealthConfigSurvivesRestore(t *testing.T) {
	image := Image{Cmd: []string{"/bin/main"}, ConfigBlob: []byte(`{"config":{"Healthcheck":{"Test":["CMD-SHELL","check"],"StartInterval":40000000,"Retries":5},"Shell":["/bin/sh","-ec"]}}`)}
	e := &Engine{images: map[string]Image{"local": image}, containers: map[string]*container{}, state: &State{dir: t.TempDir()}}
	id, err := e.create("worker", Config{Image: "local", Healthcheck: &healthConfig{Retries: 2}})
	if err != nil {
		t.Fatal(err)
	}
	restored, err := e.state.restore()
	if err != nil {
		t.Fatal(err)
	}
	c := restored[id]
	if c.healthcheck.Retries != 2 || c.healthcheck.StartInterval != 40*time.Millisecond ||
		!slices.Equal(c.healthcheck.command(c.shell), []string{"/bin/sh", "-ec", "check"}) {
		t.Fatalf("unexpected restored health configuration: %+v, %v", c.healthcheck, c.shell)
	}
}

func TestHealthGracePeriodOnlyAppliesWhileStarting(t *testing.T) {
	check := &healthConfig{StartPeriod: time.Minute, Retries: 2}
	h := containerHealth{Status: healthStarting}
	for range 7 {
		h.record(healthResult{ExitCode: 1}, time.Second, check)
	}
	if h.Status != healthStarting || h.FailingStreak != 0 || len(h.Log) != 5 {
		t.Fatalf("unexpected health during grace period: %+v", h)
	}
	h.record(healthResult{}, 2*time.Second, check)
	h.record(healthResult{ExitCode: 1}, 3*time.Second, check)
	h.record(healthResult{ExitCode: 1}, 4*time.Second, check)
	if h.Status != healthUnhealthy || h.FailingStreak != 2 || h.Log[2].ExitCode != 0 {
		t.Fatalf("unexpected health after first success: %+v", h)
	}
}

// Pausing while the guest acknowledges a probe must wait for that startup
// instead of failing the user's pause with a container-operation conflict.
func TestPauseWaitsForHealthProbeStartup(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := &container{id: "running", state: "running", health: &containerHealth{Status: healthStarting}}
		run := c.nextAttempt()
		run.healthChanged = make(chan struct{})
		x := &execProcess{container: c, containerRun: run, healthProbe: run.healthChanged,
			spec: guestwire.ExecSpec{ID: "probe", ContainerID: c.id, Argv: []string{"check"}}}
		e := &Engine{ctx: t.Context(), containers: map[string]*container{c.id: c}, execs: map[string]*execProcess{x.spec.ID: x}}
		host, guest := net.Pipe()
		defer guest.Close()
		e.session = guestwire.NewSession(host, e.onEvent)
		defer e.session.Close()
		_ = guest.SetDeadline(time.Now().Add(time.Second))
		started := make(chan error, 1)
		go func() {
			_, _, err := e.startExec(t.Context(), x.spec.ID, false, [2]uint16{}, true)
			started <- err
		}()
		frame, err := guestwire.ReadFrame(guest, nil)
		if err != nil || frame.Kind != guestwire.ExecMessage {
			t.Fatalf("probe request: %+v, %v", frame, err)
		}
		paused := make(chan int, 1)
		go func() {
			response := httptest.NewRecorder()
			e.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/containers/running/pause", nil))
			paused <- response.Code
		}()
		synctest.Wait()
		select {
		case status := <-paused:
			t.Fatalf("pause returned HTTP %d before the probe started", status)
		default:
		}
		frame, err = (guestwire.RunEvent{Kind: guestwire.StartedMessage, ID: x.spec.ID, PID: 9}).Frame()
		if err != nil {
			t.Fatal(err)
		}
		if err := guestwire.WriteFrame(guest, frame); err != nil {
			t.Fatal(err)
		}
		if err := <-started; err != nil {
			t.Fatal(err)
		}
		frame, err = guestwire.ReadFrame(guest, nil)
		if err != nil || frame.Kind != guestwire.FreezeMessage {
			t.Fatalf("pause request: %+v, %v", frame, err)
		}
		if err := guestwire.WriteFrame(guest, guestwire.Frame{Kind: guestwire.AckMessage}); err != nil {
			t.Fatal(err)
		}
		if status := <-paused; status != http.StatusNoContent || !c.paused {
			t.Fatalf("pause returned HTTP %d, paused %t", status, c.paused)
		}
	})
}
