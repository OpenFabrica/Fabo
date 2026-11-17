package main

import (
	"bufio"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/OpenFabrica/Fabo/engine/internal/winhost"
)

// The host is setup's setupSystem: what setup does to the PC.

// arch is the PC's architecture: the bundle's engine and Fabo Linux are
// this build's, so an x64 build on an Arm PC is refused by verified files
// not matching rather than by guessing here.
func (h *host) arch() string {
	if native := winhost.NativeArch(); native != "" {
		return native
	}
	return runtime.GOARCH
}

// wslInstalled reports whether `wsl --version` answers (the Store WSL).
func (h *host) wslInstalled() bool {
	return winhost.Quiet("wsl.exe", "--version").Run() == nil
}

// distros lists the WSL distros of this user.
func (h *host) distros() []string {
	out, err := winhost.Quiet("wsl.exe", "-l", "-q").Output()
	if err != nil {
		return nil
	}
	return winhost.Distros(out)
}

// bootTime is when Windows last started.
func (h *host) bootTime() time.Time { return winhost.BootTime() }

// otherDocker reports another Docker on this PC: Docker Desktop, or a
// docker CLI on PATH that is not Fabo's.
func (h *host) otherDocker() bool {
	if pf := os.Getenv("ProgramFiles"); pf != "" {
		if _, err := os.Stat(filepath.Join(pf, `Docker\Docker\Docker Desktop.exe`)); err == nil {
			return true
		}
	}
	ours := filepath.Join(h.install, "bin")
	for _, d := range filepath.SplitList(os.Getenv("PATH")) {
		if d != "" && !strings.EqualFold(filepath.Clean(d), filepath.Clean(ours)) {
			if _, err := os.Stat(filepath.Join(d, "docker.exe")); err == nil {
				return true
			}
		}
	}
	return false
}

// installWSL runs fabo-admin.ps1 -Wsl as administrator.
func (h *host) installWSL(msi, sha256 string) (uint32, error) {
	script, err := h.verified("fabo-admin.ps1")
	if err != nil {
		return 0, err
	}
	code, err := winhost.ElevatedPowerShell(script, "-Wsl", "-Msi", msi, "-Sha256", sha256)
	if err != nil {
		h.logf("setup: the elevated PowerShell did not start (%v)", err)
		return 0, winhost.ErrDeclined
	}
	return code, nil
}

// resumeAfterRestart opens Fabo once after Windows restarts.
func (h *host) resumeAfterRestart() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	return winhost.SetRun(`Software\Microsoft\Windows\CurrentVersion\RunOnce`, "FaboSetup", `"`+exe+`"`)
}

// restartWindows restarts the PC now (setup's "Restart now").
func (h *host) restartWindows() error {
	return winhost.Quiet("shutdown.exe", "/r", "/t", "0").Run()
}

// importDistro imports a root filesystem archive as a WSL 2 distro.
func (h *host) importDistro(name, dir, tarball string) error {
	out, err := winhost.Quiet("wsl.exe", "--import", name, dir, tarball, "--version", "2").CombinedOutput()
	if err != nil {
		return errors.New(winhost.FirstLine(out, "wsl --import failed"))
	}
	return nil
}

// installUbuntu installs the downloaded image as a real Ubuntu (in Start
// too); a WSL too old for --from-file imports it instead.
func (h *host) installUbuntu(file, dir string) error {
	if winhost.Quiet("wsl.exe", "--install", "--from-file", file, "--name", ubuntuDistro, "--no-launch").Run() == nil {
		return nil
	}
	return h.importDistro(ubuntuDistro, dir, file)
}

// installEngine runs install.sh as root in distro, after checking it and
// the binaries it copies.
func (h *host) installEngine(distro string) error {
	for _, f := range []string{"fabo", "fabo-daemon", "fabo-init", "fabo-start"} {
		if _, err := h.verified(f); err != nil {
			return err
		}
	}
	script, err := h.verified("install.sh")
	if err != nil {
		return err
	}
	path, err := h.toWSL(distro, script)
	if err != nil {
		return err
	}
	return h.wslRun(distro, "sh", path)
}

// startFaboLinux starts the engine in Fabo Linux.
func (h *host) startFaboLinux() error {
	return h.wslRun(winhost.Distro, winhost.WSLStart)
}

// setDistro carries connections to distro from now on.
func (h *host) setDistro(string) { h.mux.Reset() }

// testContainer runs the test container with the bundle's docker.exe.
func (h *host) testContainer() (string, error) {
	docker, err := h.verified("bin/docker.exe")
	if err != nil {
		return "", err
	}
	host := "npipe://" + strings.ReplaceAll(h.pipe, `\`, "/")
	c := winhost.Quiet(docker, "--host", host, "run", "--rm", "alpine", "echo", "hello")
	c.Env = append(os.Environ(), "DOCKER_CONFIG="+filepath.Join(h.dir, "setup-docker"))
	out, err := c.CombinedOutput()
	return string(out), err
}

// runTools runs setup-tools.ps1, which prints "TASK <n>" as each task
// finishes and "ERROR <why>" when one fails.
func (h *host) runTools(makeDefault bool, done func(int)) error {
	for _, f := range []string{"bin/docker.exe", "bin/fabo.exe", "bin/cli-plugins/docker-compose.exe", "bin/cli-plugins/docker-buildx.exe"} {
		if _, err := h.verified(f); err != nil {
			return err
		}
	}
	script, err := h.verified("setup-tools.ps1")
	if err != nil {
		return err
	}
	args := []string{"-Bundle", h.install}
	if makeDefault {
		args = append(args, "-Default")
	}
	c := winhost.PowerShellFile(script, args...)
	stdout, err := c.StdoutPipe()
	if err != nil {
		return err
	}
	// stderr is drained as it comes: docker writes progress there, and a
	// full pipe would stall the script.
	var stderr strings.Builder
	c.Stderr = &limitedWriter{w: &stderr, n: 64 << 10}
	if err := c.Start(); err != nil {
		return err
	}
	why := ""
	sc := bufio.NewScanner(stdout)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if n, ok := strings.CutPrefix(line, "TASK "); ok {
			if i, err := strconv.Atoi(n); err == nil {
				done(i)
			}
		} else if e, ok := strings.CutPrefix(line, "ERROR "); ok {
			why = strings.TrimSpace(e)
		}
	}
	if err := c.Wait(); err != nil {
		if why == "" {
			lines := nonEmptyLines(stderr.String())
			for i := len(lines) - 1; i >= 0; i-- {
				if !strings.Contains(lines[i], "Successfully") {
					why = lines[i]
					break
				}
			}
		}
		if why == "" {
			why = "installing the Docker tools failed"
		}
		return errors.New(why)
	}
	return nil
}

// limitedWriter keeps at most n bytes.
type limitedWriter struct {
	w io.Writer
	n int
}

// Write keeps what fits and drops the rest.
func (l *limitedWriter) Write(p []byte) (int, error) {
	if l.n > 0 {
		k := min(len(p), l.n)
		l.n -= k
		_, _ = l.w.Write(p[:k])
	}
	return len(p), nil
}

// setAutostart sets or removes Fabo's sign-in entry, and the RunOnce entry
// setup left for a restart.
func (h *host) setAutostart(on bool) error {
	_ = winhost.SetRun(`Software\Microsoft\Windows\CurrentVersion\RunOnce`, "FaboSetup", "")
	command := ""
	if on {
		exe, err := os.Executable()
		if err != nil {
			return err
		}
		// A command line, not a Go string: the path in plain double quotes.
		command = `"` + exe + `" --background`
	}
	return winhost.SetRun(winhost.RunKey, winhost.RunValue, command)
}

// verified is a bundle file checked against this build's manifest.
func (h *host) verified(rel string) (string, error) {
	return h.manifest.Verified(h.install, rel, h.release)
}
