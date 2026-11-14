//go:build linux || darwin

package localengine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/OpenFabrica/FabriBay/engine/internal/guestwire"
)

type statsSample struct {
	guestwire.ContainerStats
	read time.Time
}

var errStatsRunEnded = errors.New("container run ended")

func (e *Engine) statsHTTP(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	stream := !q.Has("stream") || flagSet(q, "stream")
	oneShot := flagSet(q, "one-shot")
	if stream && oneShot {
		writeError(w, fmt.Errorf("%w: one-shot requires stream=false", errInvalid))
		return
	}
	e.mu.Lock()
	c, err := e.find(r.PathValue("id"))
	var run *runAttempt
	var name string
	if err == nil {
		name = c.name
		if c.state == "running" {
			run = c.attempt
		}
	}
	e.mu.Unlock()
	if err != nil {
		writeError(w, err)
		return
	}
	if run == nil {
		writeJSON(w, http.StatusOK, containerStatsJSON(c.id, name, statsSample{}, statsSample{}, 0))
		return
	}
	first, err := e.readContainerStats(r.Context(), c, run)
	if errors.Is(err, errStatsRunEnded) {
		writeJSON(w, http.StatusOK, containerStatsJSON(c.id, name, statsSample{}, statsSample{}, 0))
		return
	}
	if err != nil {
		writeError(w, err)
		return
	}
	ceiling := e.statsMemoryLimit()
	if !stream {
		if !oneShot {
			timer := time.NewTimer(500 * time.Millisecond)
			defer timer.Stop()
			select {
			case <-r.Context().Done():
				return
			case <-run.done:
			case <-timer.C:
				if second, err := e.readContainerStats(r.Context(), c, run); err == nil {
					writeJSON(w, http.StatusOK, containerStatsJSON(c.id, name, second, first, ceiling))
					return
				}
			}
		}
		writeJSON(w, http.StatusOK, containerStatsJSON(c.id, name, first, statsSample{}, ceiling))
		return
	}

	w.Header().Set("Content-Type", "application/json")
	out := json.NewEncoder(w)
	controller := http.NewResponseController(w)
	defer controller.SetWriteDeadline(time.Time{})
	send := func(now, previous statsSample) error {
		_ = controller.SetWriteDeadline(time.Now().Add(5 * time.Second))
		if err := out.Encode(containerStatsJSON(c.id, name, now, previous, ceiling)); err != nil {
			return err
		}
		return controller.Flush()
	}
	if err := send(first, statsSample{}); err != nil {
		return
	}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	previous := first
	for {
		select {
		case <-r.Context().Done():
			return
		case <-run.done:
			return
		case <-ticker.C:
			now, err := e.readContainerStats(r.Context(), c, run)
			if err != nil || send(now, previous) != nil {
				return
			}
			previous = now
		}
	}
}

func (e *Engine) readContainerStats(ctx context.Context, c *container, run *runAttempt) (statsSample, error) {
	select {
	case <-run.done:
		return statsSample{}, errStatsRunEnded
	default:
	}
	data, err := e.session.ContainerStats(ctx, c.id)
	select {
	case <-run.done:
		// A restart may have replaced the cgroup while the request waited
		// for the guest slot. Never combine counters from different runs.
		return statsSample{}, errStatsRunEnded
	default:
	}
	return statsSample{ContainerStats: data, read: time.Now().UTC()}, err
}

func (e *Engine) statsMemoryLimit() uint64 {
	if e.vmMemory != 0 {
		return e.vmMemory << 20
	}
	raw, _ := os.ReadFile("/proc/meminfo")
	for line := range strings.SplitSeq(string(raw), "\n") {
		if raw, ok := strings.CutPrefix(line, "MemTotal:"); ok {
			fields := strings.Fields(raw)
			if len(fields) != 0 {
				value, _ := strconv.ParseUint(fields[0], 10, 64)
				return value * 1024
			}
		}
	}
	return 0
}

func containerStatsJSON(id, name string, now, previous statsSample, ceiling uint64) map[string]any {
	cpu := func(s statsSample) map[string]any {
		return map[string]any{
			"cpu_usage": map[string]uint64{
				"total_usage": s.CPUUsageUsec * 1000, "usage_in_kernelmode": s.CPUSystemUsec * 1000,
				"usage_in_usermode": s.CPUUserUsec * 1000,
			},
			// Linux /proc/stat accounts CPU time in USER_HZ (100 ticks/s).
			"system_cpu_usage": s.SystemTicks * (uint64(time.Second) / 100),
			"online_cpus":      s.OnlineCPUs,
		}
	}
	limit := now.MemoryMax
	if limit == 0 || ceiling != 0 && limit > ceiling {
		limit = ceiling
	}
	return map[string]any{
		"id": id, "name": "/" + name, "read": now.read, "preread": previous.read,
		"pids_stats": map[string]uint32{"current": now.PIDs},
		"cpu_stats":  cpu(now), "precpu_stats": cpu(previous),
		"memory_stats": map[string]any{
			"usage": now.MemoryCurrent, "limit": limit, "stats": map[string]uint64{"inactive_file": now.MemoryInactiveFile},
		},
		"networks": map[string]any{"eth0": map[string]uint64{
			"rx_bytes": now.RXBytes, "rx_packets": now.RXPackets, "tx_bytes": now.TXBytes, "tx_packets": now.TXPackets,
		}},
		"blkio_stats": map[string]any{"io_service_bytes_recursive": []map[string]any{
			{"op": "read", "value": now.IOReadBytes},
			{"op": "write", "value": now.IOWriteBytes},
		}},
	}
}
