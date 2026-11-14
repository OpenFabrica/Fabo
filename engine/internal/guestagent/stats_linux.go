package guestagent

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/OpenFabrica/FabriBay/engine/internal/guestwire"
)

func (r *Runtime) containerStats(id string) (guestwire.ContainerStats, error) {
	r.mu.Lock()
	child := r.children[id]
	r.mu.Unlock()
	if child == nil || child.isExec {
		return guestwire.ContainerStats{}, fmt.Errorf("container is not running")
	}
	if child.cgroup == "" {
		return guestwire.ContainerStats{}, fmt.Errorf("container statistics require a delegated cgroup v2 directory")
	}

	dir := child.cgroup
	cpu, err := os.ReadFile(filepath.Join(dir, "cpu.stat"))
	if err != nil {
		return guestwire.ContainerStats{}, err
	}
	now, err := BoottimeNS()
	if err != nil {
		return guestwire.ContainerStats{}, err
	}
	s := guestwire.ContainerStats{
		ReadNS:             now,
		MemoryCurrent:      counter(readText(filepath.Join(dir, "memory.current"))),
		MemoryMax:          counter(readText(filepath.Join(dir, "memory.max"))),
		MemoryInactiveFile: field(readText(filepath.Join(dir, "memory.stat")), "inactive_file"),
		CPUUsageUsec:       field(string(cpu), "usage_usec"),
		CPUUserUsec:        field(string(cpu), "user_usec"),
		CPUSystemUsec:      field(string(cpu), "system_usec"),
	}
	tasks := readText(filepath.Join(dir, "pids.current"))
	if tasks != "" {
		s.PIDs = uint32(counter(tasks))
	} else {
		s.PIDs = uint32(len(strings.Fields(readText(filepath.Join(dir, "cgroup.threads")))))
	}
	procStat, err := os.ReadFile("/proc/stat")
	if err != nil {
		return guestwire.ContainerStats{}, err
	}
	for line := range strings.SplitSeq(string(procStat), "\n") {
		label, values, _ := strings.Cut(line, " ")
		if label == "cpu" {
			ticks := strings.Fields(values)
			// Guest and guest_nice are already included in user and nice.
			for _, value := range ticks[:min(8, len(ticks))] {
				s.SystemTicks += counter(value)
			}
		} else if strings.HasPrefix(label, "cpu") {
			s.OnlineCPUs++
		}
	}
	netDev := readText(fmt.Sprintf("/proc/%d/net/dev", child.Process.Pid))
	for line := range strings.SplitSeq(netDev, "\n") {
		if name, values, ok := strings.Cut(line, ":"); ok && strings.TrimSpace(name) != "lo" {
			columns := strings.Fields(values)
			if len(columns) >= 10 {
				s.RXBytes += counter(columns[0])
				s.RXPackets += counter(columns[1])
				s.TXBytes += counter(columns[8])
				s.TXPackets += counter(columns[9])
			}
		}
	}
	for item := range strings.FieldsSeq(readText(filepath.Join(dir, "io.stat"))) {
		if value, ok := strings.CutPrefix(item, "rbytes="); ok {
			s.IOReadBytes += counter(value)
		} else if value, ok := strings.CutPrefix(item, "wbytes="); ok {
			s.IOWriteBytes += counter(value)
		}
	}
	return s, nil
}

func counter(raw string) uint64 {
	value, _ := strconv.ParseUint(strings.TrimSpace(raw), 10, 64)
	return value
}

// BoottimeNS reads the same Linux clock as the original guest.
func BoottimeNS() (uint64, error) {
	var ts unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_BOOTTIME, &ts); err != nil {
		return 0, err
	}
	return uint64(ts.Sec)*1_000_000_000 + uint64(ts.Nsec), nil
}

func sample() (guestwire.Stats, error) {
	now, err := BoottimeNS()
	if err != nil {
		return guestwire.Stats{}, err
	}
	mem := readText("/proc/meminfo")
	statm := strings.Fields(readText("/proc/self/statm"))
	var rss uint64
	if len(statm) > 1 {
		pages, _ := strconv.ParseUint(statm[1], 10, 64)
		rss = pages * uint64(os.Getpagesize()) / 1024
	}
	return guestwire.Stats{
		UptimeNS: now, MemoryTotalKiB: field(mem, "MemTotal:"),
		MemoryAvailableKiB: field(mem, "MemAvailable:"), AgentRSSKiB: rss,
		MemoryUsedKiB: heldMemoryKiB(), MemoryStall: pressure(readText("/proc/pressure/memory")),
		OOMKills: field(readText("/proc/vmstat"), "oom_kill"),
	}, nil
}

func readText(path string) string {
	raw, _ := os.ReadFile(path)
	return string(raw)
}

func field(text, key string) uint64 {
	for line := range strings.SplitSeq(text, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == key {
			value, _ := strconv.ParseUint(fields[1], 10, 64)
			return value
		}
	}
	return 0
}

func pressure(text string) uint64 {
	for line := range strings.SplitSeq(text, "\n") {
		if !strings.HasPrefix(line, "full ") {
			continue
		}
		for _, field := range strings.Fields(line) {
			if raw, ok := strings.CutPrefix(field, "avg10="); ok {
				if value, err := strconv.ParseFloat(raw, 64); err == nil && value >= 0 && value <= 100 {
					return uint64(math.Round(value * 100))
				}
			}
		}
	}
	return 0
}

func heldMemoryKiB() uint64 {
	for line := range strings.SplitSeq(readText("/proc/self/cgroup"), "\n") {
		relative, ok := strings.CutPrefix(line, "0::")
		if !ok {
			continue
		}
		dir := filepath.Join("/sys/fs/cgroup", filepath.Clean("/"+relative))
		stat := readText(filepath.Join(dir, "memory.stat"))
		kernel := field(stat, "kernel")
		if kernel == 0 {
			for _, key := range []string{"kernel_stack", "pagetables", "percpu", "sock", "vmalloc", "slab"} {
				kernel += field(stat, key)
			}
		}
		return (field(stat, "anon") + field(stat, "shmem") + kernel) / 1024
	}
	return 0
}
