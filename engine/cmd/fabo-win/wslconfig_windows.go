package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/OpenFabrica/Fabo/engine/internal/winhost"
)

// Settings → Resources: WSL's CPUs and memory in %USERPROFILE%\.wslconfig
// (the file logic is winhost/wslconfig.go). Upstream Kiri's
// crates/kiri-win/src/wslconfig.rs.

// wslConfigPath is %USERPROFILE%\.wslconfig.
func wslConfigPath() (string, error) {
	home := os.Getenv("USERPROFILE")
	if home == "" {
		return "", errors.New("USERPROFILE is not set")
	}
	return filepath.Join(home, ".wslconfig"), nil
}

// memoryStatusEx is Win32's MEMORYSTATUSEX.
type memoryStatusEx struct {
	length               uint32
	memoryLoad           uint32
	totalPhys            uint64
	availPhys            uint64
	totalPageFile        uint64
	availPageFile        uint64
	totalVirtual         uint64
	availVirtual         uint64
	availExtendedVirtual uint64
}

// globalMemoryStatusEx is kernel32's, loaded from System32 only.
var globalMemoryStatusEx = windows.NewLazySystemDLL("kernel32.dll").NewProc("GlobalMemoryStatusEx")

// physicalMiB is the PC's memory in MiB, 0 when Windows does not say.
func physicalMiB() int {
	m := memoryStatusEx{length: uint32(unsafe.Sizeof(memoryStatusEx{}))}
	if r, _, _ := globalMemoryStatusEx.Call(uintptr(unsafe.Pointer(&m))); r == 0 {
		return 0
	}
	return int(m.totalPhys >> 20)
}

// distroRunning reports whether the engine's distro runs now, asked in a
// way that never starts WSL (`wsl -l --running`), so reading Settings or
// making a report does not wake a VM the user left stopped.
func (h *host) distroRunning() bool {
	distro := h.distro()
	if distro == "" {
		return false
	}
	out, err := winhost.Quiet("wsl.exe", "-l", "--running", "-q").Output()
	if err != nil {
		return false
	}
	for _, name := range winhost.Distros(out) {
		if strings.EqualFold(name, distro) {
			return true
		}
	}
	return false
}

// runningVM is what WSL's VM has now, CPUs and MiB as Linux reports them,
// or nil while the engine's distro does not run.
func (h *host) runningVM() *[2]int {
	if !h.distroRunning() {
		return nil
	}
	out, err := h.wsl(h.distro(), "sh", "-c", "nproc; head -1 /proc/meminfo").Output()
	if err != nil {
		return nil
	}
	cpus, mib, ok := winhost.ParseRunning(string(out))
	if !ok {
		return nil
	}
	return &[2]int{cpus, mib}
}

// resources is GET /resources.
func (h *host) resources() (winhost.WSLResources, error) {
	path, err := wslConfigPath()
	if err != nil {
		return winhost.WSLResources{}, err
	}
	text, err := winhost.ReadWSLConfigText(path)
	if err != nil {
		return winhost.WSLResources{}, err
	}
	return winhost.ReadWSLResources(text, runtime.NumCPU(), physicalMiB(), h.runningVM()), nil
}

// setResources is POST /resources: processors and memory into .wslconfig,
// the rest of the file as it was.
func (h *host) setResources(cpus, mib int) error {
	if err := winhost.CheckWSLResources(cpus, mib, runtime.NumCPU(), physicalMiB()); err != nil {
		return invalidError{err}
	}
	path, err := wslConfigPath()
	if err != nil {
		return err
	}
	changed, err := winhost.SaveWSLResources(path, cpus, mib)
	if err != nil {
		h.logf("resources: .wslconfig not saved: %v", err)
		return fmt.Errorf("could not save .wslconfig: %w", err)
	}
	if changed {
		h.logf("resources: .wslconfig now has processors=%d memory=%s", cpus, winhost.MemoryValue(mib))
	}
	return nil
}

// restartWSL is POST /resources/restart.
func (h *host) restartWSL() error {
	h.logf("resources: restarting WSL for .wslconfig")
	if err := h.engine.userRestartWSL(); err != nil {
		h.logf("resources: WSL restart: %v", err)
		return fmt.Errorf("could not restart WSL: %w", err)
	}
	return nil
}

// shutdownWSL stops every WSL distro and the VM, and drops the bridge that
// ran in it, so the next connection starts a new one.
func (h *host) shutdownWSL() error {
	out, err := winhost.Quiet("wsl.exe", "--shutdown").CombinedOutput()
	if err != nil {
		return errors.New(winhost.FirstLine(out, "wsl --shutdown failed: "+err.Error()))
	}
	if h.mux != nil {
		h.mux.Reset()
	}
	return nil
}
