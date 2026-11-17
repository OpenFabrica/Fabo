package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/windows"

	"github.com/OpenFabrica/Fabo/engine/internal/redact"
	"github.com/OpenFabrica/Fabo/engine/internal/winhost"
)

// Settings → Diagnostics: a redacted report on the Desktop and the logs
// folder in Explorer (the report's text is winhost/diag.go). Upstream
// Kiri's crates/kiri-win/src/diag.rs.

// explorer is Windows' explorer.exe, which lives in the Windows folder,
// not System32.
func explorer() string {
	root := os.Getenv("SystemRoot")
	if root == "" {
		root = `C:\Windows`
	}
	return filepath.Join(root, "explorer.exe")
}

// windowsVersion is `ver`'s line, e.g. Microsoft Windows [Version 10.0.26100.2314].
func windowsVersion() string {
	out, err := winhost.Quiet("cmd.exe", "/c", "ver").Output()
	if v := strings.TrimSpace(string(out)); err == nil && v != "" {
		return v
	}
	return "Windows"
}

// engineRaw reads path from the engine through Fabo's pipe, as raw bytes.
func (h *host) engineRaw(path string) ([]byte, error) {
	c := &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return winhost.DialPipe(h.pipe, 2*time.Second)
		}},
	}
	resp, err := c.Get("http://docker" + path)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err == nil && resp.StatusCode >= 300 {
		err = fmt.Errorf("the engine answered %s", resp.Status)
	}
	return b, err
}

// diagReport is POST /diag/report: the report saved to the Desktop and
// shown in Explorer. The path it returns has the user's name taken out.
func (h *host) diagReport(containers bool) (string, error) {
	d := winhost.DiagReport{Version: release, OS: windowsVersion(), Arch: h.arch(), WithContainers: containers}
	if runtime.GOARCH != d.Arch {
		d.Arch += " (" + runtime.GOARCH + " build)"
	}
	var err error
	if d.WindowLog, err = winhost.TailFile(filepath.Join(h.dir, "fabo-win.log"), winhost.DiagTailBytes); err != nil {
		d.WindowNote = err.Error()
	}
	var secrets []string
	running := h.distroRunning()
	switch distro := h.distro(); {
	case distro == "":
		d.EngineNote = "Fabo is not set up yet"
	case !running:
		// Reading it would start WSL; a stopped distro has no new lines.
		d.EngineNote = "WSL is not running"
	default:
		out, err := h.wsl(distro, "sh", "-c", winhost.EngineLogScript).Output()
		if err != nil {
			// What wsl.exe printed on failure is its own UTF-16 text, not the log.
			d.EngineNote = "could not read the engine's log in WSL: " + err.Error()
		} else {
			d.EngineLog = winhost.DropCutLine(strings.ToValidUTF8(string(out), "\uFFFD"), len(out) >= winhost.DiagTailBytes)
		}
		// The signed-in share token, to take out wherever it appears.
		if b, err := h.wsl(distro, "cat", winhost.WSLShareConfig).Output(); err == nil {
			if token := winhost.ShareToken(b); token != "" {
				secrets = append(secrets, token)
			}
		}
	}
	if containers {
		if !running {
			d.ContainersNote = "the engine is not running"
		} else if body, err := h.engineRaw("/containers/json?all=1"); err != nil {
			d.ContainersNote = err.Error()
		} else if d.Containers, err = winhost.ContainerLines(body); err != nil {
			d.ContainersNote = err.Error()
		}
	}
	home := os.Getenv("USERPROFILE")
	text := d.Text(redact.New(home, secrets))

	dir, err := windows.KnownFolderPath(windows.FOLDERID_Desktop, 0)
	if err != nil || dir == "" {
		if home == "" {
			return "", errors.New("could not find the Desktop folder")
		}
		dir = filepath.Join(home, "Desktop")
	}
	path := filepath.Join(dir, winhost.ReportName(time.Now()))
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		h.logf("report not saved: %v", err)
		return "", fmt.Errorf("could not save the report: %w", err)
	}
	h.logf("report saved to the Desktop")
	// explorer.exe parses its own command line: /select, then the quoted path.
	c := exec.Command(explorer())
	c.SysProcAttr = &syscall.SysProcAttr{CmdLine: `explorer.exe /select,"` + path + `"`}
	if err := c.Start(); err == nil {
		go c.Wait()
	}
	return redact.New(home, nil).Apply(path), nil
}

// openLogs is POST /diag/folder: %LOCALAPPDATA%\Fabo, where fabo-win.log is.
func (h *host) openLogs() error {
	c := exec.Command(explorer(), h.dir)
	if err := c.Start(); err != nil {
		return err
	}
	go c.Wait()
	return nil
}
