//go:build linux || darwin

package localengine

import (
	"net/http"
	"runtime"
)

func (e *Engine) infoHTTP(w http.ResponseWriter, _ *http.Request) {
	awake := e.remoteGuest
	select {
	case <-e.settled:
		awake = false
	default:
	}
	e.WriteInfo(w, awake, e.remoteGuest && !awake)
}

// WriteInfo adds the supervisor's VM lifecycle state to the engine snapshot.
func (e *Engine) WriteInfo(w http.ResponseWriter, awake, stopped bool) {
	e.mu.Lock()
	running, paused, total, images := 0, 0, len(e.containers), len(e.allImages())
	for _, c := range e.containers {
		switch c.dockerState() {
		case "running":
			running++
		case "paused":
			paused++
		}
	}
	e.mu.Unlock()
	result := map[string]any{
		"ID": "fabo", "Name": "fabo", "ServerVersion": e.version(), "FaboEngine": "go",
		"Containers": total, "ContainersRunning": running, "ContainersPaused": paused, "ContainersStopped": total - running - paused, "Images": images,
		"Driver": "overlayfs", "OperatingSystem": "Fabo", "OSType": "linux", "Architecture": runtime.GOARCH,
		"CgroupDriver": "cgroupfs", "CgroupVersion": "2", "Warnings": []string{},
		"FaboVMControls": e.remoteGuest, "FaboExecStop": true,
		"FaboRosetta": e.rosetta,
	}
	if e.remoteGuest {
		result["FaboVmAwake"], result["FaboStopped"] = awake, stopped
		if awake {
			result["NCPU"], result["MemTotal"] = e.vmCPUs, e.vmMemory<<20
		}
	}
	writeJSON(w, http.StatusOK, result)
}
