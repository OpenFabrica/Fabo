package main

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/OpenFabrica/Fabo/engine/internal/winhost"
)

var (
	msiDLL                       = windows.NewLazySystemDLL("msi.dll")
	procMsiOpenDatabase          = msiDLL.NewProc("MsiOpenDatabaseW")
	procMsiDatabaseOpenView      = msiDLL.NewProc("MsiDatabaseOpenViewW")
	procMsiViewExecute           = msiDLL.NewProc("MsiViewExecute")
	procMsiViewFetch             = msiDLL.NewProc("MsiViewFetch")
	procMsiRecordGetString       = msiDLL.NewProc("MsiRecordGetStringW")
	procMsiCloseHandle           = msiDLL.NewProc("MsiCloseHandle")
	procMsiGetSummaryInformation = msiDLL.NewProc("MsiGetSummaryInformationW")
	procMsiSummaryInfoGetProp    = msiDLL.NewProc("MsiSummaryInfoGetPropertyW")
)

// newUpdater is the host's updater, with the feed this build carries.
func newUpdater(h *host) *updater {
	return newUpdaterFor(h, h.dir, updateURL, release)
}

// msiClose closes an MSI handle.
func msiClose(h uintptr) { procMsiCloseHandle.Call(h) }

// msiProperty reads one row of an MSI's Property table.
func msiProperty(path, name string) (string, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return "", err
	}
	var db uintptr
	// MSIDBOPEN_READONLY is 0, passed as a pointer value.
	if r, _, _ := procMsiOpenDatabase.Call(uintptr(unsafe.Pointer(p)), 0, uintptr(unsafe.Pointer(&db))); r != 0 {
		return "", fmt.Errorf("not a Windows Installer package (error %d)", r)
	}
	defer msiClose(db)
	q, _ := windows.UTF16PtrFromString("SELECT `Value` FROM `Property` WHERE `Property`='" + name + "'")
	var view uintptr
	if r, _, _ := procMsiDatabaseOpenView.Call(db, uintptr(unsafe.Pointer(q)), uintptr(unsafe.Pointer(&view))); r != 0 {
		return "", fmt.Errorf("the package has no Property table (error %d)", r)
	}
	defer msiClose(view)
	if r, _, _ := procMsiViewExecute.Call(view, 0); r != 0 {
		return "", fmt.Errorf("reading the package failed (error %d)", r)
	}
	var rec uintptr
	if r, _, _ := procMsiViewFetch.Call(view, uintptr(unsafe.Pointer(&rec))); r != 0 {
		return "", fmt.Errorf("the package has no %s", name)
	}
	defer msiClose(rec)
	buf := make([]uint16, 256)
	n := uint32(len(buf))
	if r, _, _ := procMsiRecordGetString.Call(rec, 1, uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&n))); r != 0 {
		return "", fmt.Errorf("reading %s failed (error %d)", name, r)
	}
	return windows.UTF16ToString(buf[:n]), nil
}

// msiTemplate reads the summary information's Template ("x64;1033").
func msiTemplate(path string) (string, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return "", err
	}
	var info uintptr
	if r, _, _ := procMsiGetSummaryInformation.Call(0, uintptr(unsafe.Pointer(p)), 0, uintptr(unsafe.Pointer(&info))); r != 0 {
		return "", fmt.Errorf("reading the package's summary failed (error %d)", r)
	}
	defer msiClose(info)
	const pidTemplate = 7
	var typ, ival uint32
	var ft windows.Filetime
	buf := make([]uint16, 128)
	n := uint32(len(buf))
	if r, _, _ := procMsiSummaryInfoGetProp.Call(info, pidTemplate, uintptr(unsafe.Pointer(&typ)), uintptr(unsafe.Pointer(&ival)),
		uintptr(unsafe.Pointer(&ft)), uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&n))); r != 0 {
		return "", fmt.Errorf("reading the package's platform failed (error %d)", r)
	}
	return windows.UTF16ToString(buf[:n]), nil
}

// checkMSI is whether msi is a Fabo installer for this PC's platform, of
// the version the feed promised: a swapped feed could otherwise point at
// any MSI, or at an older, genuine one and its known bugs.
func (h *host) checkMSI(path, version string) error {
	code, err := msiProperty(path, "UpgradeCode")
	if err != nil {
		return err
	}
	if !strings.EqualFold(code, upgradeCode) {
		return errors.New("the update is not a Fabo installer")
	}
	template, err := msiTemplate(path)
	if err != nil {
		return err
	}
	if !platformMatches(template, h.arch()) {
		return fmt.Errorf("the update is for another kind of PC (%s)", template)
	}
	got, err := msiProperty(path, "ProductVersion")
	if err != nil {
		return err
	}
	// Windows Installer versions are numbers only: 0.6.0-rc.1 is 0.6.0.
	want, _, _ := strings.Cut(version, "-")
	if got != want || !newer(version, release) {
		return fmt.Errorf("the update is version %s, not the newer %s it promised", got, version)
	}
	return nil
}

// startHelper runs fabo.ps1 -Update (installed beside fabo-win, checked
// against the build's manifest), which waits for this process to end.
func (h *host) startHelper(msi string) error {
	script, err := h.verified("fabo.ps1")
	if err != nil {
		return err
	}
	args := append(append([]string{}, winhost.PowerShellArgs...), script, "-Update", "-Msi", msi, "-WaitFor", strconv.Itoa(os.Getpid()))
	return winhost.Detached(winhost.Tool("powershell.exe"), args...)
}
