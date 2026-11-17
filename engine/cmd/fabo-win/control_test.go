package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/OpenFabrica/Fabo/engine/internal/buildwith"
	"github.com/OpenFabrica/Fabo/engine/internal/winhost"
)

// fakeController records what the control API asked.
type fakeController struct {
	started, quitted bool
	refreshed        bool
	action           string
	value            string
	quitCh           chan struct{}
	cpus, mib        int
	restarted        bool
	containers       bool
	opened           bool
	namesOn          bool
	with             string
}

func (f *fakeController) status() hostStatus    { return hostStatus{Version: "1.2.3", Engine: "running"} }
func (f *fakeController) startEngine() error    { f.started = true; return nil }
func (f *fakeController) stopEngine() error     { return errors.New("WSL is not answering") }
func (f *fakeController) restartEngine() error  { return nil }
func (f *fakeController) quit()                 { close(f.quitCh) }
func (f *fakeController) setupState() setupView { return setupView{Step: "choose"} }
func (f *fakeController) setupAction(a string, v json.RawMessage) error {
	f.action, f.value = a, string(v)
	if a == "nope" {
		return errUnknownAction
	}
	return nil
}
func (f *fakeController) updateState() updateView   { return updateView{State: "idle"} }
func (f *fakeController) updateAction(string) error { return errBusy }
func (f *fakeController) refreshProxy()             { f.refreshed = true }

// domains answers whether names were switched on.
func (f *fakeController) domains() domainsView {
	return domainsView{On: f.namesOn, Suffix: "fabo.local", Installed: true, Serving: true}
}

// setDomains switches names, busy when asked to turn on twice.
func (f *fakeController) setDomains(on bool) error {
	if on && f.namesOn {
		return errNamesBusy
	}
	f.namesOn = on
	return nil
}

// usbState lists one device in Fabo.
func (f *fakeController) usbState(context.Context) usbView {
	return usbView{Devices: []usbDevice{{BusID: "1-3", Name: "CP2102", InFabo: true}}}
}

// usbAction records the action; bus 9-9 is a declined prompt.
func (f *fakeController) usbAction(_ context.Context, action, bus string) (usbResult, error) {
	f.action, f.value = "usb "+action, bus
	switch action {
	case "attach":
		return usbResult{Name: "CP2102", Cancelled: bus == "9-9"}, nil
	case "detach":
		return usbResult{}, errors.New("usbipd failed")
	}
	return usbResult{}, errUnknownAction
}

// buildWith answers as fabo-win does beside Docker Desktop, asking until chosen.
func (f *fakeController) buildWith() buildwith.Status {
	return buildwith.Status{With: f.with, Ask: f.with == "", Engine: "Docker Desktop", Ready: true}
}

// chooseBuildWith records the choice.
func (f *fakeController) chooseBuildWith(with string) error { f.with = with; return nil }
func (f *fakeController) resources() (winhost.WSLResources, error) {
	return winhost.WSLResources{CPUs: 4, Memory: 8192, MaxCPUs: 8, MaxMemory: 16384}, nil
}
func (f *fakeController) setResources(cpus, mib int) error {
	f.cpus, f.mib = cpus, mib
	if cpus > 8 {
		return invalidError{errors.New("CPUs must be between 1 and 8")}
	}
	return nil
}
func (f *fakeController) restartWSL() error { f.restarted = true; return nil }
func (f *fakeController) diagReport(containers bool) (string, error) {
	f.containers = containers
	return `~\Desktop\Fabo report.txt`, nil
}
func (f *fakeController) openLogs() error { f.opened = true; return nil }

// TestControlAPI checks the routes the window uses and their answers.
func TestControlAPI(t *testing.T) {
	c := &fakeController{quitCh: make(chan struct{})}
	srv := httptest.NewServer(controlHandler(c))
	defer srv.Close()
	get := func(path string) string {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return string(b)
	}
	post := func(path, body string) (int, string) {
		resp, err := http.Post(srv.URL+path, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if s := get("/status"); !strings.Contains(s, `"version":"1.2.3"`) || !strings.Contains(s, `"engine":"running"`) {
		t.Fatalf("status = %s", s)
	}
	if code, _ := post("/engine/start", ""); code != http.StatusNoContent || !c.started {
		t.Fatalf("start = %d", code)
	}
	if code, body := post("/engine/stop", ""); code != http.StatusInternalServerError || !strings.Contains(body, "WSL is not answering") {
		t.Fatalf("stop = %d %s", code, body)
	}
	if code, _ := post("/engine/bogus", ""); code != http.StatusNotFound {
		t.Fatalf("bogus = %d", code)
	}
	if code, _ := post("/setup/choose", `{"value":"fabo"}`); code != http.StatusNoContent || c.action != "choose" || c.value != `"fabo"` {
		t.Fatalf("choose = %d %q %q", code, c.action, c.value)
	}
	if code, _ := post("/setup/nope", ""); code != http.StatusNotFound {
		t.Fatalf("unknown setup action = %d", code)
	}
	if code, _ := post("/update/download", ""); code != http.StatusConflict {
		t.Fatalf("busy update = %d", code)
	}
	if resp, _ := http.Get(srv.URL + "/engine/start"); resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET on a POST route = %d", resp.StatusCode)
	}
	if code, _ := post("/domains/on", ""); code != http.StatusNoContent || !c.namesOn {
		t.Fatalf("domains on = %d", code)
	}
	if s := get("/domains"); !strings.Contains(s, `"on":true`) || !strings.Contains(s, `"suffix":"fabo.local"`) {
		t.Fatalf("domains = %s", s)
	}
	if code, _ := post("/domains/on", ""); code != http.StatusConflict {
		t.Fatalf("busy domains = %d", code)
	}
	if code, _ := post("/domains/maybe", ""); code != http.StatusNotFound {
		t.Fatalf("bogus domains = %d", code)
	}
	if code, _ := post("/proxy", ""); code != http.StatusNoContent || !c.refreshed {
		t.Fatalf("proxy = %d", code)
	}
	if s := get("/usb"); !strings.Contains(s, `"busid":"1-3"`) || !strings.Contains(s, `"inFabo":true`) {
		t.Fatalf("usb = %s", s)
	}
	if code, body := post("/usb/attach", `{"value":"1-3"}`); code != http.StatusOK || c.action != "usb attach" || c.value != "1-3" || !strings.Contains(body, `"name":"CP2102"`) {
		t.Fatalf("usb attach = %d %s %q %q", code, body, c.action, c.value)
	}
	if code, body := post("/usb/attach", `{"value":"9-9"}`); code != http.StatusOK || !strings.Contains(body, `"cancelled":true`) {
		t.Fatalf("usb attach declined = %d %s", code, body)
	}
	if code, body := post("/usb/detach", `{"value":"1-3"}`); code != http.StatusInternalServerError || !strings.Contains(body, "usbipd failed") {
		t.Fatalf("usb detach = %d %s", code, body)
	}
	if code, _ := post("/usb/eject", ""); code != http.StatusNotFound {
		t.Fatalf("unknown usb action = %d", code)
	}
	if s := get("/buildwith"); !strings.Contains(s, `"ask":true`) || !strings.Contains(s, `"engine":"Docker Desktop"`) {
		t.Fatalf("buildwith = %s", s)
	}
	if code, _ := post("/buildwith", `{"value":"fabo"}`); code != http.StatusNoContent || c.with != "fabo" {
		t.Fatalf("choose fabo = %d %q", code, c.with)
	}
	if code, _ := post("/buildwith", `{"value":"podman"}`); code != http.StatusBadRequest || c.with != "fabo" {
		t.Fatalf("choose podman = %d", code)
	}
	if code, _ := post("/buildwith", ""); code != http.StatusBadRequest {
		t.Fatalf("no body = %d", code)
	}
	if s := get("/resources"); !strings.Contains(s, `"cpus":4`) || !strings.Contains(s, `"max_memory_mib":16384`) {
		t.Fatalf("resources = %s", s)
	}
	if code, _ := post("/resources", `{"cpus":2,"memory_mib":4096}`); code != http.StatusNoContent || c.cpus != 2 || c.mib != 4096 {
		t.Fatalf("set resources = %d %d %d", code, c.cpus, c.mib)
	}
	if code, body := post("/resources", `{"cpus":99,"memory_mib":4096}`); code != http.StatusBadRequest || !strings.Contains(body, `"message":"CPUs must be between 1 and 8"`) {
		t.Fatalf("refused resources = %d %s", code, body)
	}
	if code, _ := post("/resources", `nonsense`); code != http.StatusBadRequest {
		t.Fatalf("bad body = %d", code)
	}
	if code, _ := post("/resources/restart", ""); code != http.StatusNoContent || !c.restarted {
		t.Fatalf("restart = %d", code)
	}
	if code, body := post("/diag/report", `{"containers":true}`); code != http.StatusOK || !c.containers || !strings.Contains(body, `Fabo report.txt`) {
		t.Fatalf("report = %d %s", code, body)
	}
	if code, _ := post("/diag/folder", ""); code != http.StatusNoContent || !c.opened {
		t.Fatalf("folder = %d", code)
	}
	post("/quit", "")
	select {
	case <-c.quitCh:
	case <-time.After(5 * time.Second):
		t.Fatal("quit not called")
	}
}
