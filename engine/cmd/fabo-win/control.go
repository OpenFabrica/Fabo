package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/OpenFabrica/Fabo/engine/internal/buildwith"
	"github.com/OpenFabrica/Fabo/engine/internal/winhost"
)

// hostStatus is GET /status on the control pipe: what the window shows
// about the Windows side.
type hostStatus struct {
	Version string `json:"version"`
	// SetupPending: first-run setup has not finished, so the window opens
	// on it rather than on containers.
	SetupPending bool   `json:"setupPending"`
	Distro       string `json:"distro,omitempty"`
	// Engine is "running", "starting", "stopping" or "stopped".
	Engine string `json:"engine"`
	// DockerEngine: Fabo also serves \\.\pipe\docker_engine.
	DockerEngine bool `json:"dockerEngine"`
	// OtherEngine names the engine that holds docker_engine, if any.
	OtherEngine string `json:"otherEngine,omitempty"`
	Error       string `json:"error,omitempty"`
}

// controller is what the control pipe asks of the host.
type controller interface {
	status() hostStatus
	startEngine() error
	stopEngine() error
	restartEngine() error
	quit()
	setupState() setupView
	setupAction(action string, value json.RawMessage) error
	updateState() updateView
	updateAction(action string) error
	// domains and setDomains read and switch *.fabo.local (dns.go).
	domains() domainsView
	setDomains(on bool) error
	// refreshProxy pushes Windows' proxy to the engine now.
	refreshProxy()
	usbState(ctx context.Context) usbView
	usbAction(ctx context.Context, action, busID string) (usbResult, error)
	// buildWith is which builder docker build uses; chooseBuildWith
	// switches it ("docker" or "fabo").
	buildWith() buildwith.Status
	chooseBuildWith(with string) error
	// resources reads WSL's CPUs and memory from .wslconfig, and what the
	// VM runs with now, without starting WSL.
	resources() (winhost.WSLResources, error)
	// setResources writes them to .wslconfig; WSL uses them from its next start.
	setResources(cpus, memoryMiB int) error
	// restartWSL shuts all of WSL down and starts the engine again.
	restartWSL() error
	// diagReport saves a redacted report to the Desktop and returns its path.
	diagReport(containers bool) (string, error)
	// openLogs shows Fabo's logs folder in Explorer.
	openLogs() error
}

// controlHandler serves fabo-win's control API, used by fabo-desktop on
// the control pipe (winhost.ControlPipe). Requests that change something
// are POSTs; the answers are JSON.
func controlHandler(c controller) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, c.status())
	})
	mux.HandleFunc("POST /engine/{action}", func(w http.ResponseWriter, r *http.Request) {
		var err error
		switch r.PathValue("action") {
		case "start":
			err = c.startEngine()
		case "stop":
			err = c.stopEngine()
		case "restart":
			err = c.restartEngine()
		default:
			http.NotFound(w, r)
			return
		}
		answer(w, err)
	})
	// The window saved proxy settings: a custom proxy on Windows' loopback
	// needs the relay now rather than at the next minute's push.
	mux.HandleFunc("POST /proxy", func(w http.ResponseWriter, r *http.Request) {
		c.refreshProxy()
		answer(w, nil)
	})
	mux.HandleFunc("GET /setup", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, c.setupState())
	})
	mux.HandleFunc("POST /setup/{action}", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Value json.RawMessage `json:"value"`
		}
		if r.ContentLength != 0 {
			if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
				http.Error(w, `{"message":"bad request"}`, http.StatusBadRequest)
				return
			}
		}
		answer(w, c.setupAction(r.PathValue("action"), body.Value))
	})
	mux.HandleFunc("GET /update", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, c.updateState())
	})
	mux.HandleFunc("POST /update/{action}", func(w http.ResponseWriter, r *http.Request) {
		answer(w, c.updateAction(r.PathValue("action")))
	})
	mux.HandleFunc("GET /domains", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, c.domains())
	})
	mux.HandleFunc("POST /domains/{state}", func(w http.ResponseWriter, r *http.Request) {
		switch r.PathValue("state") {
		case "on":
			answer(w, c.setDomains(true))
		case "off":
			answer(w, c.setDomains(false))
		default:
			http.NotFound(w, r)
		}
	})
	mux.HandleFunc("GET /usb", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, c.usbState(r.Context()))
	})
	// POST /usb/attach|detach with {"value":"<bus ID>"}, or /usb/install.
	// A declined administrator prompt is not an error: 200 with
	// "cancelled", so the window can stay quiet about it.
	mux.HandleFunc("POST /usb/{action}", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Value string `json:"value"`
		}
		if r.ContentLength != 0 {
			if err := json.NewDecoder(io.LimitReader(r.Body, 4<<10)).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
				http.Error(w, `{"message":"bad request"}`, http.StatusBadRequest)
				return
			}
		}
		res, err := c.usbAction(r.Context(), r.PathValue("action"), body.Value)
		if err != nil {
			answer(w, err)
			return
		}
		writeJSON(w, res)
	})
	// Settings → Builds and the window's one question about it.
	mux.HandleFunc("GET /buildwith", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, c.buildWith())
	})
	mux.HandleFunc("POST /buildwith", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Value string `json:"value"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&body); err != nil || (body.Value != buildwith.Docker && body.Value != buildwith.Fabo) {
			http.Error(w, `{"message":"build with \"docker\" or \"fabo\""}`, http.StatusBadRequest)
			return
		}
		answer(w, c.chooseBuildWith(body.Value))
	})
	mux.HandleFunc("GET /resources", func(w http.ResponseWriter, r *http.Request) {
		res, err := c.resources()
		if err != nil {
			answer(w, err)
			return
		}
		writeJSON(w, res)
	})
	mux.HandleFunc("POST /resources", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			CPUs   int `json:"cpus"`
			Memory int `json:"memory_mib"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 4<<10)).Decode(&body); err != nil {
			http.Error(w, `{"message":"bad request"}`, http.StatusBadRequest)
			return
		}
		answer(w, c.setResources(body.CPUs, body.Memory))
	})
	mux.HandleFunc("POST /resources/restart", func(w http.ResponseWriter, r *http.Request) {
		answer(w, c.restartWSL())
	})
	mux.HandleFunc("POST /diag/report", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Containers bool `json:"containers"`
		}
		if r.ContentLength != 0 {
			if err := json.NewDecoder(io.LimitReader(r.Body, 4<<10)).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
				http.Error(w, `{"message":"bad request"}`, http.StatusBadRequest)
				return
			}
		}
		path, err := c.diagReport(body.Containers)
		if err != nil {
			answer(w, err)
			return
		}
		writeJSON(w, map[string]string{"path": path})
	})
	mux.HandleFunc("POST /diag/folder", func(w http.ResponseWriter, r *http.Request) {
		answer(w, c.openLogs())
	})
	mux.HandleFunc("POST /quit", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		go c.quit()
	})
	return mux
}

// errUnknownAction is an action the host does not have.
var errUnknownAction = errors.New("unknown action")

// invalidError is a refused value: its own message, answered as 400
// like errBadRequest.
type invalidError struct{ err error }

// Error is the refusal's own message, without a prefix.
func (e invalidError) Error() string { return e.err.Error() }

// Is makes errors.Is(err, errBadRequest) hold, so answer() sends 400.
func (e invalidError) Is(target error) bool { return target == errBadRequest }

// answer is 204 on success, else the error as {"message"} with a status
// the window can tell apart.
func answer(w http.ResponseWriter, err error) {
	if err == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	status := http.StatusInternalServerError
	if errors.Is(err, errUnknownAction) {
		status = http.StatusNotFound
	} else if errors.Is(err, errBusy) || errors.Is(err, errNamesBusy) {
		status = http.StatusConflict
	} else if errors.Is(err, errBadRequest) {
		status = http.StatusBadRequest
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"message": strings.TrimSpace(err.Error())})
}

// writeJSON answers v as JSON.
func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
