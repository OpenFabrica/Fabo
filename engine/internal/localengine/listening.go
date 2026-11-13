//go:build linux || darwin

package localengine

import (
	"encoding/binary"
	"fmt"
	"maps"
	"net/http"
	"net/netip"
	"slices"
	"strconv"
	"strings"
)

func (e *Engine) listeningHTTP(w http.ResponseWriter, r *http.Request) {
	e.mu.Lock()
	c, err := e.find(r.PathValue("id"))
	var run *runAttempt
	var hasNetwork, hostNetwork bool
	if err == nil {
		if c.state != "running" || c.attempt == nil {
			err = fmt.Errorf("%w: container is not running", errConflict)
		} else {
			run, hasNetwork = c.attempt, c.networkMode() != "none" && c.networkTarget() == ""
			hostNetwork = c.networkMode() == "host"
		}
	}
	e.mu.Unlock()
	if err != nil {
		writeError(w, err)
		return
	}
	rows, err := e.session.Listening(r.Context(), c.id)
	select {
	case <-run.done:
		err = fmt.Errorf("%w: container stopped while reading its listeners", errConflict)
	default:
	}
	if err != nil {
		writeError(w, err)
		return
	}
	reachable, local := listeningPorts(rows, hasNetwork, hostNetwork)
	writeJSON(w, http.StatusOK, map[string][]uint16{"tcp": reachable, "local": local})
}

func listeningPorts(rows []string, hasNetwork, hostNetwork bool) ([]uint16, []uint16) {
	reachable, local := make(map[uint16]struct{}), make(map[uint16]struct{})
	for _, row := range rows {
		fields := strings.Fields(row)
		if len(fields) < 4 || fields[3] != "0A" {
			continue
		}
		host, port, ok := strings.Cut(fields[1], ":")
		n, err := strconv.ParseUint(port, 16, 16)
		if !ok || err != nil || n == 0 {
			continue
		}
		ip := procAddress(host)
		if !ip.IsValid() {
			continue
		}
		reachesTarget := ip.IsUnspecified() || ip.Is4() && !ip.IsLoopback()
		if hostNetwork {
			reachesTarget = ip.Is4() && (ip.IsUnspecified() || ip == netip.AddrFrom4([4]byte{127, 0, 0, 1}))
		}
		if hasNetwork && reachesTarget {
			reachable[uint16(n)] = struct{}{}
		} else {
			local[uint16(n)] = struct{}{}
		}
	}
	for port := range reachable {
		delete(local, port)
	}
	return append([]uint16{}, slices.Sorted(maps.Keys(reachable))...), append([]uint16{}, slices.Sorted(maps.Keys(local))...)
}

func procAddress(value string) netip.Addr {
	if len(value) != 8 && len(value) != 32 {
		return netip.Addr{}
	}
	var address [16]byte
	// Both supported Linux guest architectures print little-endian words.
	for i := 0; i < len(value); i += 8 {
		word, err := strconv.ParseUint(value[i:i+8], 16, 32)
		if err != nil {
			return netip.Addr{}
		}
		binary.LittleEndian.PutUint32(address[i/2:], uint32(word))
	}
	ip, _ := netip.AddrFromSlice(address[:len(value)/2])
	return ip.Unmap()
}
