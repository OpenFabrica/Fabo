package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// On Windows the window talks to fabo-win, the host that serves the Docker
// API and runs the engine in WSL, on its control pipe \\.\pipe\fabo-win
// (engine/cmd/fabo-win/control.go): first-run setup, engine start and
// stop, updates, quitting. Only the dial is Windows-specific
// (engine_windows.go), so this client and the setup page are tested here.

// hostStatus is fabo-win's GET /status.
type hostStatus struct {
	Version      string `json:"version"`
	SetupPending bool   `json:"setupPending"`
	Distro       string `json:"distro"`
	Engine       string `json:"engine"`
	DockerEngine bool   `json:"dockerEngine"`
	OtherEngine  string `json:"otherEngine"`
}

// hostTask is one line of a setup step's checklist.
type hostTask struct {
	Label  string `json:"label"`
	State  string `json:"state"`
	Detail string `json:"detail"`
}

// hostProgress is a setup step's bar.
type hostProgress struct {
	Pct   *int64 `json:"pct"`
	Done  *int64 `json:"done"`
	Total *int64 `json:"total"`
	Rate  *int64 `json:"rate"`
	ETA   *int64 `json:"eta"`
	Label string `json:"label"`
}

// hostSetup is fabo-win's GET /setup.
type hostSetup struct {
	Step           string           `json:"step"`
	Linux          string           `json:"linux"`
	Sizes          map[string]int64 `json:"sizes"`
	ExistingUbuntu string           `json:"existingUbuntu"`
	OtherDocker    bool             `json:"otherDocker"`
	MakeDefault    bool             `json:"makeDefault"`
	Autostart      bool             `json:"autostart"`
	Busy           bool             `json:"busy"`
	Progress       *hostProgress    `json:"progress"`
	Tasks          []hostTask       `json:"tasks"`
	Log            []string         `json:"log"`
	Error          string           `json:"error"`
}

// hostUpdate is fabo-win's GET /update.
type hostUpdate struct {
	State    string `json:"state"`
	Current  string `json:"current"`
	Latest   string `json:"latest"`
	NotesURL string `json:"notesUrl"`
	Done     int64  `json:"done"`
	Total    int64  `json:"total"`
	Error    string `json:"error"`
}

// hostClient calls fabo-win's control API.
type hostClient struct{ http *http.Client }

// newHostClient returns a client dialing with dial, or nil without one
// (macOS and Linux have no host process).
func newHostClient(dial func(context.Context) (net.Conn, error)) *hostClient {
	if dial == nil {
		return nil
	}
	return &hostClient{http: &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) { return dial(ctx) },
	}}}
}

// call sends one request and decodes a JSON answer into out (may be nil).
func (h *hostClient) call(ctx context.Context, method, path string, body any, out any) error {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://fabo-win"+path, reader)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := h.http.Do(req)
	if err != nil {
		return errors.New(tr("Fabo 的 Windows 服务没有运行，请从开始菜单重新打开 Fabo", "Fabo's Windows service is not running: open Fabo again from the Start menu"))
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		var e struct{ Message string }
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		if json.Unmarshal(raw, &e) == nil && e.Message != "" {
			return errors.New(e.Message)
		}
		return fmt.Errorf("%s %s: %s", method, path, strings.TrimSpace(string(raw)))
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

// status reads the host's status.
func (h *hostClient) status(ctx context.Context) (hostStatus, error) {
	var st hostStatus
	return st, h.call(ctx, http.MethodGet, "/status", nil, &st)
}

// setup reads first-run setup's state.
func (h *hostClient) setup(ctx context.Context) (hostSetup, error) {
	var v hostSetup
	return v, h.call(ctx, http.MethodGet, "/setup", nil, &v)
}

// setupAction sends one setup action with an optional value.
func (h *hostClient) setupAction(ctx context.Context, action string, value any) error {
	var body any
	if value != nil {
		body = map[string]any{"value": value}
	}
	return h.call(ctx, http.MethodPost, "/setup/"+action, body, nil)
}

// engine starts, stops or restarts the engine in WSL.
func (h *hostClient) engine(ctx context.Context, action string) error {
	return h.call(ctx, http.MethodPost, "/engine/"+action, nil, nil)
}

// update reads the updater's state.
func (h *hostClient) update(ctx context.Context) (hostUpdate, error) {
	var u hostUpdate
	return u, h.call(ctx, http.MethodGet, "/update", nil, &u)
}

// updateAction checks, downloads, installs or cancels an update.
func (h *hostClient) updateAction(ctx context.Context, action string) error {
	return h.call(ctx, http.MethodPost, "/update/"+action, nil, nil)
}

// quit ends fabo-win, which also stops serving docker.
func (h *hostClient) quit() {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = h.call(ctx, http.MethodPost, "/quit", nil, nil)
}

// hostEngineAction maps the window's engine actions to the host's: the
// engine in WSL has no VM to wake or stop, so fabo-win starts and stops it.
func hostEngineAction(path string) string {
	switch path {
	case "/fabo/wake":
		return "start"
	case "/fabo/stop":
		return "stop"
	}
	return ""
}
