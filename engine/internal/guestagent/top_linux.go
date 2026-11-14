package guestagent

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"golang.org/x/sys/unix"

	"github.com/OpenFabrica/FabriBay/engine/internal/guestwire"
)

func (r *Runtime) top(id string) (guestwire.Frame, error) {
	r.mu.Lock()
	child := r.children[id]
	r.mu.Unlock()
	if child == nil || child.isExec {
		return guestwire.Frame{}, fmt.Errorf("container is not running")
	}
	if child.cgroup == "" {
		return guestwire.Frame{}, fmt.Errorf("container process listing requires a delegated cgroup v2 directory")
	}
	pids, err := cgroupProcesses(child.cgroup)
	if err != nil {
		return guestwire.Frame{}, err
	}
	uptime, err := BoottimeNS()
	if err != nil {
		return guestwire.Frame{}, err
	}
	now := time.Now()
	boot := now.Add(-time.Duration(uptime))
	lines := make(map[int]string, len(pids))
	size := 4 // list count
	for pid := range pids {
		if row := processRow(pid, boot, now); row != "" {
			size += 4 + len(row)
			if size > guestwire.MaxFrame {
				return guestwire.Frame{}, fmt.Errorf("container process list exceeds the guest frame size")
			}
			lines[pid] = row
		}
	}
	// Ignore processes that exited or left the container while /proc was read.
	current, err := cgroupProcesses(child.cgroup)
	if err != nil {
		return guestwire.Frame{}, err
	}
	var rows []string
	for _, pid := range slices.Sorted(maps.Keys(lines)) {
		if current[pid] {
			rows = append(rows, lines[pid])
		}
	}
	return guestwire.TopFrame(rows)
}

func cgroupProcesses(dir string) (map[int]bool, error) {
	pids := make(map[int]bool)
	err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			if path != dir && errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if !entry.IsDir() {
			return nil
		}
		raw, err := os.ReadFile(filepath.Join(path, "cgroup.procs"))
		if err != nil {
			if path != dir && errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		for field := range strings.FieldsSeq(string(raw)) {
			if pid, err := strconv.Atoi(field); err == nil && pid > 0 {
				pids[pid] = true
			}
		}
		return nil
	})
	return pids, err
}

func processRow(pid int, boot, now time.Time) string {
	// Read all fields via a pinned process directory so they cannot
	// accidentally describe different processes after a PID is reused.
	proc, err := os.OpenRoot(filepath.Join("/proc", strconv.Itoa(pid)))
	if err != nil {
		return ""
	}
	defer proc.Close()
	raw, err := proc.ReadFile("stat")
	if err != nil {
		return ""
	}
	stat := string(raw)
	open, close := strings.IndexByte(stat, '('), strings.LastIndexByte(stat, ')')
	if open < 0 || close <= open {
		return ""
	}
	fields := strings.Fields(stat[close+1:])
	if len(fields) < 20 {
		return ""
	}
	status, err := proc.ReadFile("status")
	if err != nil {
		return ""
	}
	user := ""
	for line := range strings.SplitSeq(string(status), "\n") {
		if raw, ok := strings.CutPrefix(line, "Uid:"); ok {
			if ids := strings.Fields(raw); len(ids) > 0 {
				user = ids[0]
			}
			break
		}
	}
	if user == "0" {
		user = "root"
	}
	cmd := processCommand(proc)
	if cmd == "" {
		cmd = "[" + cleanProcessText(stat[open+1:close]) + "]"
	}
	const tick = 10 * time.Millisecond // Linux USER_HZ
	start := boot.Add(time.Duration(counter(fields[19])) * tick)
	cpu := time.Duration(counter(fields[11])+counter(fields[12])) * tick
	percent := int(float64(cpu) * 100 / float64(max(tick, now.Sub(start))))
	secs := int64(cpu / time.Second)
	cpuTime := fmt.Sprintf("%02d:%02d:%02d", secs/3600, secs/60%60, secs%60)
	tty := "?"
	if n, err := strconv.ParseInt(fields[4], 10, 32); err == nil && n != 0 {
		dev := uint64(uint32(n))
		major, minor := unix.Major(dev), unix.Minor(dev)
		if major >= 136 && major <= 143 {
			tty = fmt.Sprintf("pts/%d", (major-136)*256+minor)
		} else {
			tty = fmt.Sprintf("%d:%d", major, minor)
		}
	}
	return fmt.Sprintf("%s\t%d\t%s\t%d\t%s\t%s\t%s\t%s", user, pid, fields[1], percent,
		start.UTC().Format("15:04"), tty, cpuTime, cmd)
}

func processCommand(proc *os.Root) string {
	f, err := proc.Open("cmdline")
	if err != nil {
		return ""
	}
	defer f.Close()
	const maxCommand = 8 << 10
	raw, err := io.ReadAll(io.LimitReader(f, maxCommand+1))
	if err != nil {
		return ""
	}
	cmd := cleanProcessText(string(raw[:min(len(raw), maxCommand)]))
	if len(raw) > maxCommand {
		cmd += "…"
	}
	return cmd
}

func cleanProcessText(raw string) string {
	return strings.TrimSpace(strings.Map(func(ch rune) rune {
		if unicode.IsControl(ch) {
			return ' '
		}
		return ch
	}, raw))
}
