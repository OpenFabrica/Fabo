package guestagent

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/OpenFabrica/Fabo/engine/internal/guestwire"
)

func (r *Runtime) freeze(id string, frozen bool) error {
	r.mu.Lock()
	child := r.children[id]
	r.mu.Unlock()
	if child == nil || child.isExec {
		return fmt.Errorf("container is not running")
	}
	if child.cgroup == "" {
		return fmt.Errorf("container has no cgroup; pause requires a delegated cgroup v2 directory")
	}

	control := filepath.Join(child.cgroup, "cgroup.freeze")
	previous, err := os.ReadFile(control)
	if err != nil {
		return err
	}
	value := "0"
	if frozen {
		value = "1"
	}
	if err := os.WriteFile(control, []byte(value), 0); err != nil {
		return err
	}
	// The freezer write starts a transition. Only the events file
	// confirms that all processes have stopped or resumed.
	deadline := time.Now().Add(time.Second)
	for {
		events, err := os.ReadFile(filepath.Join(child.cgroup, "cgroup.events"))
		if err != nil {
			return errors.Join(err, os.WriteFile(control, previous, 0))
		}
		if strings.Contains("\n"+string(events), "\nfrozen "+value+"\n") {
			return nil
		}
		if !time.Now().Before(deadline) {
			return errors.Join(fmt.Errorf("container freezer did not reach state %s", value), os.WriteFile(control, previous, 0))
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func cgroupLimits(l guestwire.Limits) bool {
	return l.MemoryMax != 0 || l.CPUQuota != 0 || l.CPUPeriod != 0 || l.CPUWeight != 0 || l.PIDsMax != 0
}

type cgroupSetting struct{ controller, file, value string }

func limitChanges(before, after guestwire.Limits) []cgroupSetting {
	var settings []cgroupSetting
	maxValue := func(n uint64) string {
		if n == 0 {
			return "max"
		}
		return strconv.FormatUint(n, 10)
	}
	if before.CPUQuota != after.CPUQuota || before.CPUPeriod != after.CPUPeriod {
		period := after.CPUPeriod
		if period == 0 {
			period = 100000
		}
		settings = append(settings, cgroupSetting{"cpu", "cpu.max", fmt.Sprintf("%s %d", maxValue(after.CPUQuota), period)})
	}
	if before.CPUWeight != after.CPUWeight {
		weight := after.CPUWeight
		if weight == 0 {
			weight = 100
		}
		settings = append(settings, cgroupSetting{"cpu", "cpu.weight", strconv.FormatUint(uint64(weight), 10)})
	}
	if before.PIDsMax != after.PIDsMax {
		settings = append(settings, cgroupSetting{"pids", "pids.max", maxValue(uint64(after.PIDsMax))})
	}
	if before.MemoryMax != after.MemoryMax {
		// A memory reduction may trigger reclaim, so apply it last.
		settings = append(settings, cgroupSetting{"memory", "memory.max", maxValue(after.MemoryMax)})
	}
	return settings
}

func (r *Runtime) updateLimits(id string, limits guestwire.Limits) error {
	r.mu.Lock()
	child := r.children[id]
	r.mu.Unlock()
	if child == nil || child.isExec {
		return fmt.Errorf("container is not running")
	}
	if child.cgroup == "" {
		return fmt.Errorf("resource updates require a delegated cgroup v2 directory")
	}
	settings := limitChanges(child.limits, limits)
	previous := make([][]byte, len(settings))
	for i, setting := range settings {
		raw, err := os.ReadFile(filepath.Join(child.cgroup, setting.file))
		if err != nil {
			return fmt.Errorf("read %s: %w", setting.file, err)
		}
		previous[i] = raw
	}
	for i, setting := range settings {
		if err := os.WriteFile(filepath.Join(child.cgroup, setting.file), []byte(setting.value), 0); err != nil {
			failure := fmt.Errorf("set %s: %w", setting.file, err)
			for j := i - 1; j >= 0; j-- {
				if err := os.WriteFile(filepath.Join(child.cgroup, settings[j].file), previous[j], 0); err != nil {
					failure = errors.Join(failure, fmt.Errorf("restore %s: %w", settings[j].file, err))
				}
			}
			return failure
		}
	}
	child.limits = limits
	return nil
}

func createCgroup(parent, id string, limits guestwire.Limits) (*os.File, error) {
	if parent == "" {
		return nil, fmt.Errorf("resource limits require a delegated FABO_CGROUP_DIR")
	}
	var controllers []string
	settings := limitChanges(guestwire.Limits{}, limits)
	for _, setting := range settings {
		controller := "+" + setting.controller
		if !slices.Contains(controllers, controller) {
			controllers = append(controllers, controller)
		}
	}
	if len(controllers) != 0 {
		if err := os.WriteFile(filepath.Join(parent, "cgroup.subtree_control"), []byte(strings.Join(controllers, " ")), 0); err != nil {
			return nil, fmt.Errorf("enable cgroup controllers: %w", err)
		}
	}
	// Accounting also needs the controllers for runs without explicit
	// limits. A delegated parent can offer only a subset of them.
	subtree := filepath.Join(parent, "cgroup.subtree_control")
	enabled := strings.Fields(readText(subtree))
	available := strings.Fields(readText(filepath.Join(parent, "cgroup.controllers")))
	for _, controller := range []string{"cpu", "memory", "pids", "cpuset", "io"} {
		if slices.Contains(available, controller) && !slices.Contains(enabled, controller) {
			_ = os.WriteFile(subtree, []byte("+"+controller), 0)
		}
	}
	dir := filepath.Join(parent, "fabo-"+id)
	if err := os.Mkdir(dir, 0755); err != nil {
		return nil, err
	}
	for _, item := range settings {
		if err := os.WriteFile(filepath.Join(dir, item.file), []byte(item.value), 0); err != nil {
			_ = os.Remove(dir)
			return nil, fmt.Errorf("set %s: %w", item.file, err)
		}
	}
	f, err := os.Open(dir)
	if err != nil {
		_ = os.Remove(dir)
	}
	return f, err
}

// removeNestedCgroups removes empty cgroups after the container's PID
// namespace exits. cgroupfs exposes virtual files which cannot be unlinked.
func removeNestedCgroups(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() {
			if err := removeNestedCgroups(filepath.Join(dir, entry.Name())); err != nil {
				return err
			}
		}
	}
	return os.Remove(dir)
}
