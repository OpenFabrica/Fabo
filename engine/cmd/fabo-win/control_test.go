package main

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fakeController records what the control API asked.
type fakeController struct {
	started, quitted bool
	action           string
	value            string
	quitCh           chan struct{}
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
	post("/quit", "")
	select {
	case <-c.quitCh:
	case <-time.After(5 * time.Second):
		t.Fatal("quit not called")
	}
}
