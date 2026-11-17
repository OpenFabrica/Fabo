package winhost

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// createNoWindow keeps a console window from flashing up for a tool.
const createNoWindow = 0x08000000

// errorCancelled is ShellExecuteEx's answer when the user declines the
// administrator prompt.
const errorCancelled = 1223

// HardenDLLSearch loads DLLs only from System32 from now on, never from the
// folder fabo-win runs from, where a planted DLL would otherwise load.
func HardenDLLSearch() {
	_ = windows.SetDefaultDllDirectories(windows.LOAD_LIBRARY_SEARCH_SYSTEM32)
}

// System32 is Windows' own tool folder.
func System32() string {
	if d, err := windows.GetSystemDirectory(); err == nil && d != "" {
		return d
	}
	root := os.Getenv("SystemRoot")
	if root == "" {
		root = `C:\Windows`
	}
	return filepath.Join(root, "System32")
}

// Tool is the full path of a Windows tool. exec.Command would search the
// running program's folder and the current directory first, so a wsl.exe
// or powershell.exe planted beside fabo-win would run in its place.
func Tool(name string) string {
	if strings.EqualFold(name, "powershell.exe") {
		return filepath.Join(System32(), `WindowsPowerShell\v1.0\powershell.exe`)
	}
	return filepath.Join(System32(), name)
}

// Quiet is a command for a Windows tool (by its System32 name) or a full
// path, with no console window.
func Quiet(name string, args ...string) *exec.Cmd {
	path := name
	if !filepath.IsAbs(name) {
		path = Tool(name)
	}
	c := exec.Command(path, args...)
	c.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}
	return c
}

// PowerShellArgs are the only way Fabo runs PowerShell: a script file from
// its own folder, checked against the build's manifest, with parameters as
// arguments. Never script text this binary carries: an encoded script in a
// hidden PowerShell is what droppers do, and Defender quarantined an
// upstream build for it.
var PowerShellArgs = []string{"-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File"}

// PowerShellFile runs script with args, quietly.
func PowerShellFile(script string, args ...string) *exec.Cmd {
	return Quiet("powershell.exe", append(append(append([]string{}, PowerShellArgs...), script), args...)...)
}

// shellExecuteInfo is SHELLEXECUTEINFOW.
type shellExecuteInfo struct {
	size         uint32
	mask         uint32
	hwnd         windows.Handle
	verb         *uint16
	file         *uint16
	parameters   *uint16
	directory    *uint16
	show         int32
	instApp      windows.Handle
	idList       uintptr
	class        *uint16
	keyClass     windows.Handle
	hotKey       uint32
	iconOrMonitr windows.Handle
	process      windows.Handle
}

var (
	shell32            = windows.NewLazySystemDLL("shell32.dll")
	procShellExecuteEx = shell32.NewProc("ShellExecuteExW")
	kernel32           = windows.NewLazySystemDLL("kernel32.dll")
	procGetTickCount64 = kernel32.NewProc("GetTickCount64")
	procPeekNamedPipe  = kernel32.NewProc("PeekNamedPipe")
)

// ErrDeclined is an administrator prompt the user turned down.
var ErrDeclined = errors.New("cancelled: Windows' approval was declined")

// ElevatedPowerShell runs a script file as administrator (Windows asks the
// user first), hidden, and returns its exit code.
func ElevatedPowerShell(script string, args ...string) (uint32, error) {
	all := append(append(append([]string{}, PowerShellArgs...), script), args...)
	return Elevated(Tool("powershell.exe"), all...)
}

// Elevated runs program as administrator, hidden, waits for it and
// returns its exit code.
func Elevated(program string, args ...string) (uint32, error) {
	quoted := make([]string, len(args))
	for i, a := range args {
		quoted[i] = syscall.EscapeArg(a)
	}
	verb, _ := windows.UTF16PtrFromString("runas")
	file, err := windows.UTF16PtrFromString(program)
	if err != nil {
		return 0, err
	}
	params, err := windows.UTF16PtrFromString(strings.Join(quoted, " "))
	if err != nil {
		return 0, err
	}
	const seeMaskNoCloseProcess = 0x40
	const seeMaskNoAsync = 0x100
	info := shellExecuteInfo{mask: seeMaskNoCloseProcess | seeMaskNoAsync, verb: verb, file: file, parameters: params, show: windows.SW_HIDE}
	info.size = uint32(unsafe.Sizeof(info))
	if ok, _, err := procShellExecuteEx.Call(uintptr(unsafe.Pointer(&info))); ok == 0 {
		if errors.Is(err, windows.Errno(errorCancelled)) {
			return 0, ErrDeclined
		}
		return 0, fmt.Errorf("could not start %s as administrator: %w", filepath.Base(program), err)
	}
	if info.process == 0 {
		return 0, errors.New("no process to wait for")
	}
	defer windows.CloseHandle(info.process)
	if _, err := windows.WaitForSingleObject(info.process, windows.INFINITE); err != nil {
		return 0, err
	}
	var code uint32
	if err := windows.GetExitCodeProcess(info.process, &code); err != nil {
		return 0, err
	}
	return code, nil
}

// BootTime is when Windows last started.
func BootTime() time.Time {
	ms, _, _ := procGetTickCount64.Call()
	return time.Now().Add(-time.Duration(ms) * time.Millisecond)
}

// Running reports the first of names (executable names, case-insensitive)
// that a running process has.
func Running(names ...string) string {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return ""
	}
	defer windows.CloseHandle(snap)
	var e windows.ProcessEntry32
	e.Size = uint32(unsafe.Sizeof(e))
	for err = windows.Process32First(snap, &e); err == nil; err = windows.Process32Next(snap, &e) {
		exe := windows.UTF16ToString(e.ExeFile[:])
		for _, n := range names {
			if strings.EqualFold(exe, n) {
				return n
			}
		}
	}
	return ""
}

// pipeSDDL grants the pipe to this user, SYSTEM and Administrators only:
// the default descriptor would let Everyone open a read-only connection.
func pipeSDDL() (string, error) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return "", err
	}
	return "D:P(A;;GA;;;SY)(A;;GA;;;BA)(A;;GA;;;" + user.User.Sid.String() + ")", nil
}

// ListenPipe serves a message-mode pipe as Docker's own is: go-winio's
// client (the docker CLI's) then reads an empty message as EOF and sends
// one for CloseWrite, so half-closes work. Remote clients are refused. It
// fails if any process already serves the name.
func ListenPipe(name string) (net.Listener, error) {
	sddl, err := pipeSDDL()
	if err != nil {
		// Never fall back to the default descriptor.
		return nil, fmt.Errorf("the pipe's security descriptor: %w", err)
	}
	l, err := winio.ListenPipe(name, &winio.PipeConfig{
		SecurityDescriptor: sddl, MessageMode: true,
		InputBufferSize: 64 << 10, OutputBufferSize: 64 << 10,
	})
	if err != nil {
		return nil, err
	}
	return pipeListener{l}, nil
}

// pipeListener wraps accepted connections with a liveness check.
type pipeListener struct{ net.Listener }

// Accept returns the next client as a PipeConn.
func (l pipeListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &PipeConn{Conn: c}, nil
}

// PipeConn is a server's end of a message-mode pipe connection.
type PipeConn struct{ net.Conn }

// CloseWrite sends the empty message that is EOF, after the client has
// read everything written before it.
func (c *PipeConn) CloseWrite() error {
	if cw, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return errors.New("not a message-mode pipe")
}

// Alive reports whether the client still has its end open, without
// reading from it.
func (c *PipeConn) Alive() bool {
	f, ok := c.Conn.(interface{ Fd() uintptr })
	if !ok {
		return false
	}
	var avail uint32
	ok2, _, _ := procPeekNamedPipe.Call(f.Fd(), 0, 0, 0, uintptr(unsafe.Pointer(&avail)), 0)
	return ok2 != 0
}

// DialPipe connects to a pipe served on this machine.
func DialPipe(name string, timeout time.Duration) (net.Conn, error) {
	return winio.DialPipe(name, &timeout)
}

// SetRun registers command to run when this user signs in under value
// (empty command removes it).
func SetRun(key, value, command string) error {
	k, _, err := registry.CreateKey(registry.CURRENT_USER, key, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	if command == "" {
		if err := k.DeleteValue(value); err != nil && !errors.Is(err, registry.ErrNotExist) {
			return err
		}
		return nil
	}
	return k.SetStringValue(value, command)
}

// RunCommand is what value under key runs at sign-in, if anything.
func RunCommand(key, value string) string {
	k, err := registry.OpenKey(registry.CURRENT_USER, key, registry.QUERY_VALUE)
	if err != nil {
		return ""
	}
	defer k.Close()
	s, _, _ := k.GetStringValue(value)
	return s
}

// Detached starts program on its own, outside this process's job, so it
// outlives fabo-win (the window, an update helper).
func Detached(program string, args ...string) error {
	c := exec.Command(program, args...)
	const createBreakawayFromJob = 0x01000000
	c.SysProcAttr = &syscall.SysProcAttr{CreationFlags: createBreakawayFromJob | windows.CREATE_NEW_PROCESS_GROUP}
	if err := c.Start(); err != nil {
		// A job that forbids breakaway refuses the flag: start inside it.
		c = exec.Command(program, args...)
		c.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NEW_PROCESS_GROUP}
		if err := c.Start(); err != nil {
			return err
		}
	}
	return c.Process.Release()
}

// NativeArch is the PC's own architecture ("amd64" or "arm64"), which an
// x64 build under emulation on Windows on Arm does not see in GOARCH.
func NativeArch() string {
	var process, native uint16
	if err := windows.IsWow64Process2(windows.CurrentProcess(), &process, &native); err == nil {
		switch native {
		case 0xAA64: // IMAGE_FILE_MACHINE_ARM64
			return "arm64"
		case 0x8664: // IMAGE_FILE_MACHINE_AMD64
			return "amd64"
		}
	}
	return ""
}
