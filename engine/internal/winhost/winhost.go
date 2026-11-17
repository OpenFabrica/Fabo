// Package winhost holds what Fabo's Windows programs share: fabo-win.exe
// (the host that serves the Docker API on a named pipe and carries it into
// WSL2) and bin\fabo.exe (the `fabo` command). Paths, setup.json, the log,
// WSL's output and the bundle's checksums are here, written without build
// tags so they are tested on every OS; the Win32 calls are in *_windows.go.
//
// Upstream Kiri's equivalents are crates/kiri-win/src/setup.rs, serve.rs
// and win.rs.
package winhost

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf16"
)

const (
	// Pipe is Fabo's own Docker API pipe; the `fabo` docker context uses it.
	Pipe = `\\.\pipe\fabo`
	// DockerEnginePipe is Docker's default pipe, which SDKs and IDE plugins
	// open without reading docker contexts. Fabo serves it while no other
	// engine does.
	DockerEnginePipe = `\\.\pipe\docker_engine`
	// ControlPipe is fabo-win's own API for the window: setup, engine
	// start and stop, updates.
	ControlPipe = `\\.\pipe\fabo-win`
	// Distro is the WSL distro Fabo Linux is imported as.
	Distro = "fabo"
	// WSLSocket is where contrib/wsl runs the engine in any distro.
	WSLSocket = "/var/lib/fabo-daemon/.fabo/run/docker.sock"
	// WSLFabo is the Linux `fabo` in the distro, which runs `fabo bridge`.
	WSLFabo = "/usr/local/bin/fabo"
	// WSLStart starts the engine in a distro without systemd (Fabo Linux).
	WSLStart = "/usr/local/bin/fabo-start"
	// RunKey is where "start Fabo when I sign in" is kept.
	RunKey = `Software\Microsoft\Windows\CurrentVersion\Run`
	// RunValue names Fabo's entry under RunKey.
	RunValue = "Fabo"
)

// Dir is %LOCALAPPDATA%\Fabo: the MSI installs there, and setup.json, the
// log, downloads and Fabo Linux's disk live there too. FABO_WIN_DIR
// overrides it, for tests and development builds.
func Dir() string {
	if d := os.Getenv("FABO_WIN_DIR"); d != "" {
		return d
	}
	if d := os.Getenv("LOCALAPPDATA"); d != "" {
		return filepath.Join(d, "Fabo")
	}
	return "."
}

// Saved is setup's progress in setup.json, kept across restarts of Fabo
// and of Windows. fabo.ps1 reads `distro` from it when uninstalling.
type Saved struct {
	Done bool `json:"done"`
	// Linux is "fabo" (Fabo Linux) or "ubuntu".
	Linux  string `json:"linux,omitempty"`
	Distro string `json:"distro,omitempty"`
	// RestartAfter is the Unix time of a WSL install that needs Windows
	// to restart.
	RestartAfter int64 `json:"restartAfter,omitempty"`
	// LastError is why setup last stopped.
	LastError string `json:"lastError,omitempty"`
	// Autostart and MakeDefault are the choices setup offered.
	Autostart   *bool `json:"autostart,omitempty"`
	MakeDefault *bool `json:"makeDefault,omitempty"`
}

// LoadSaved reads dir's setup.json; a missing or damaged file is a fresh start.
func LoadSaved(dir string) Saved {
	var s Saved
	if b, err := os.ReadFile(filepath.Join(dir, "setup.json")); err == nil {
		_ = json.Unmarshal(b, &s)
	}
	return s
}

// SaveSaved writes dir's setup.json through a temporary file, so a crash
// never leaves half of it.
func SaveSaved(dir string, s Saved) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, "setup.json.tmp")
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, "setup.json"))
}

// Log appends one line to dir's fabo-win.log, stamped in UTC to the
// millisecond as the engine's /var/log/fabo.log is, so a moment reads the
// same in both. One write per line: separate writes from two goroutines
// would interleave.
func Log(dir, msg string) {
	_ = os.MkdirAll(dir, 0o755)
	f, err := os.OpenFile(filepath.Join(dir, "fabo-win.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.WriteString(time.Now().UTC().Format("2006-01-02T15:04:05.000Z") + " " + msg + "\n")
}

// WSLText decodes wsl.exe's own output, which is UTF-16 (little-endian)
// while what it runs in the distro prints UTF-8.
func WSLText(b []byte) string {
	bom := len(b) >= 2 && b[0] == 0xff && b[1] == 0xfe
	if bom {
		b = b[2:]
	}
	utf16le := bom || len(b) >= 2
	for i := 1; i < len(b) && i < 16; i += 2 {
		if b[i] != 0 {
			utf16le = false
			break
		}
	}
	if !utf16le {
		return string(b)
	}
	units := make([]uint16, len(b)/2)
	for i := range units {
		units[i] = uint16(b[2*i]) | uint16(b[2*i+1])<<8
	}
	return strings.ReplaceAll(string(utf16.Decode(units)), "\x00", "")
}

// Distros parses `wsl.exe -l -q`.
func Distros(out []byte) []string {
	var names []string
	for _, line := range strings.Split(WSLText(out), "\n") {
		if name := strings.TrimSpace(strings.TrimPrefix(line, "\ufeff")); name != "" {
			names = append(names, name)
		}
	}
	return names
}

// FirstLine is the first non-empty line of WSL's output, for an error.
func FirstLine(out []byte, fallback string) string {
	for _, line := range strings.Split(WSLText(out), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			return line
		}
	}
	return fallback
}

// Manifest is the SHA-256 of each bundle file, `name=hex;…`, embedded in a
// release build of fabo-win.exe by scripts/windows-bundle.sh. A file
// swapped in the install folder (another process, a drive-by download) is
// refused rather than imported into WSL, installed on PATH or run.
type Manifest string

// ErrNoManifest is a release build's answer when it carries no checksums:
// building fabo-win on its own must not quietly skip every bundle check.
var ErrNoManifest = errors.New("this build carries no checksums for its files; build it with scripts/windows-bundle.sh")

// Verified returns dir/rel after checking it against the manifest. With an
// empty manifest, a development build (release false) uses the file as is.
func (m Manifest) Verified(dir, rel string, release bool) (string, error) {
	path := filepath.Join(dir, filepath.FromSlash(rel))
	want := ""
	for _, entry := range strings.Split(string(m), ";") {
		if name, sum, ok := strings.Cut(entry, "="); ok && name == rel {
			want = sum
		}
	}
	if want == "" {
		if m == "" && !release {
			return path, nil
		}
		if m == "" {
			return "", ErrNoManifest
		}
		return "", fmt.Errorf("this build carries no checksum for %s", rel)
	}
	got, err := FileSHA256(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("%s is missing from Fabo's folder", rel)
	}
	if err != nil {
		return "", err
	}
	if got != want {
		return "", fmt.Errorf("%s in Fabo's folder does not match this build (SHA-256 %s); install Fabo again", rel, got)
	}
	return path, nil
}

// FileSHA256 is a file's SHA-256 in lower-case hex.
func FileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// WindowsPathInWSL is what `wslpath -a` makes of a Windows path in a
// distro with the default automount root, for messages and when wslpath
// cannot run. C:\Users\me → /mnt/c/Users/me.
func WindowsPathInWSL(path string) string {
	p := strings.ReplaceAll(path, `\`, "/")
	if len(p) >= 2 && p[1] == ':' {
		return "/mnt/" + strings.ToLower(p[:1]) + p[2:]
	}
	return p
}
