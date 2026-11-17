package main

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
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
	} else if errors.Is(err, errBusy) {
		status = http.StatusConflict
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
