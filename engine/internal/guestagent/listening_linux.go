package guestagent

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/libracoder/kiri/engine/internal/guestwire"
)

func (r *Runtime) listening(id string) (guestwire.Frame, error) {
	r.mu.Lock()
	child := r.children[id]
	r.mu.Unlock()
	if child == nil || child.isExec {
		return guestwire.Frame{}, fmt.Errorf("container is not running")
	}
	root, err := os.OpenRoot(fmt.Sprintf("/proc/%d", child.Process.Pid))
	if err != nil {
		return guestwire.Frame{}, err
	}
	defer root.Close()
	var owned map[string]bool
	if child.hostNetwork {
		owned, err = ownedSockets(child)
		if err != nil {
			return guestwire.Frame{}, err
		}
	}
	var rows []string
	for _, name := range []string{"net/tcp", "net/tcp6"} {
		f, err := root.Open(name)
		if name == "net/tcp6" && errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return guestwire.Frame{}, err
		}
		scanner := bufio.NewScanner(f)
		limit := len(rows) + 512
		for len(rows) < limit && scanner.Scan() {
			row := scanner.Text()
			fields := strings.Fields(row)
			if len(fields) >= 10 && fields[3] == "0A" && (owned == nil || owned[fields[9]]) {
				rows = append(rows, row)
			}
		}
		if err := errors.Join(scanner.Err(), f.Close()); err != nil {
			return guestwire.Frame{}, err
		}
	}
	return guestwire.ListeningFrame(rows)
}

// A host-network process sees the namespace's entire TCP table. Match
// socket inodes to the container's processes before exposing listeners.
func ownedSockets(child *childProcess) (map[string]bool, error) {
	pids := map[int]bool{child.Process.Pid: true}
	if child.cgroup != "" {
		var err error
		pids, err = cgroupProcesses(child.cgroup)
		if err != nil {
			return nil, err
		}
	}
	sockets := make(map[string]bool)
	for pid := range pids {
		dir := filepath.Join("/proc", strconv.Itoa(pid), "fd")
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			target, err := os.Readlink(filepath.Join(dir, entry.Name()))
			if err != nil {
				continue
			}
			inode, ok := strings.CutPrefix(target, "socket:[")
			if inode, closed := strings.CutSuffix(inode, "]"); ok && closed {
				sockets[inode] = true
			}
		}
	}
	return sockets, nil
}
