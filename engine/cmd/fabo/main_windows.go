// fabo on Windows is bin\fabo.exe, the `fabo` command in PowerShell, cmd
// and Git Bash. See windows_route.go for where each command goes.
package main

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/windows"

	"github.com/OpenFabrica/Fabo/engine/internal/winhost"
)

// dockerHost is Fabo's pipe as DOCKER_HOST spells it: npipe:////./pipe/fabo.
var dockerHost = "DOCKER_HOST=npipe://" + strings.ReplaceAll(winhost.Pipe, `\`, "/")

// release is this build's version, stamped by scripts/windows-bundle.sh.
var release = "0.0.0-dev"

func main() {
	code, err := runWindows(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, "fabo:", err)
		if code == 0 {
			code = 1
		}
	}
	os.Exit(code)
}

// runWindows carries out one command and returns its exit code.
func runWindows(args []string) (int, error) {
	switch routeWindows(args) {
	case routeVersion:
		fmt.Println("fabo", release)
		return 0, nil
	case routeWindow:
		return 0, openFabo()
	case routeDocker:
		docker, err := besideDocker()
		if err != nil {
			return 1, err
		}
		return runTool(docker, args, dockerHost)
	case routeShell:
		docker, err := besideDocker()
		if err != nil {
			return 1, err
		}
		return runTool(docker, windowsShellArgs(args[2], args[3:], isConsole(os.Stdin)), dockerHost)
	case routeWSL:
		distro := winhost.LoadSaved(winhost.Dir()).Distro
		if distro == "" {
			return 1, errors.New("Fabo isn't set up yet. Open Fabo from the Start menu first")
		}
		return runTool(winhost.Tool("wsl.exe"), wslArgs(distro, args))
	case routeEngine:
		return 0, controlPost("/engine/" + args[1])
	case routeDomains:
		method, path, err := windowsDomains(args[1:])
		if err != nil {
			return 2, err
		}
		if method == http.MethodGet {
			return 0, controlGet(path, os.Stdout)
		}
		return 0, controlPost(path)
	case routeUnsupported:
		return 2, fmt.Errorf("fabo %s is not available on Windows", args[0])
	}
	fmt.Fprint(os.Stderr, windowsUsage)
	return 2, nil
}

// installDir is the folder fabo-win.exe is in: bin\fabo.exe's parent's.
func installDir() string {
	exe, err := os.Executable()
	if err != nil {
		return winhost.Dir()
	}
	return filepath.Dir(filepath.Dir(exe))
}

// besideDocker is the docker.exe installed beside this fabo.exe.
func besideDocker() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	docker := filepath.Join(filepath.Dir(exe), "docker.exe")
	if _, err := os.Stat(docker); err != nil {
		return "", errors.New("no docker.exe beside fabo.exe; reinstall Fabo")
	}
	return docker, nil
}

// runTool runs program with this console and returns its exit code.
func runTool(program string, args []string, env ...string) (int, error) {
	// Ctrl+C reaches the child too, which decides what it means (docker
	// run -it passes it on); this process just waits for its exit code.
	signal.Ignore(os.Interrupt)
	c := exec.Command(program, args...)
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	c.Env = append(os.Environ(), env...)
	err := c.Run()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode(), nil
	}
	if err != nil {
		return 1, err
	}
	return 0, nil
}

// isConsole reports whether f is a console (an interactive terminal).
func isConsole(f *os.File) bool {
	var mode uint32
	return windows.GetConsoleMode(windows.Handle(f.Fd()), &mode) == nil
}

// openFabo starts fabo-win.exe, which opens the window (or shows the
// running one's).
func openFabo() error {
	host := filepath.Join(installDir(), "fabo-win.exe")
	if _, err := os.Stat(host); err != nil {
		return errors.New("fabo-win.exe is not installed beside bin\\fabo.exe")
	}
	return winhost.Detached(host)
}

// controlGet copies the running fabo-win's answer to path into w.
func controlGet(path string, w io.Writer) error {
	c := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{DialContext: winhostDial}}
	resp, err := c.Get("http://fabo-win" + path)
	if err != nil {
		return errors.New("Fabo is not running: open Fabo first")
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return errors.New(strings.TrimSpace(jsonMessage(msg)))
	}
	_, err = io.Copy(w, resp.Body)
	return err
}

// controlPost asks the running fabo-win to act.
func controlPost(path string) error {
	c := &http.Client{
		Timeout:   3 * time.Minute,
		Transport: &http.Transport{DialContext: winhostDial},
	}
	resp, err := c.Post("http://fabo-win"+path, "application/json", nil)
	if err != nil {
		return errors.New("Fabo is not running: open Fabo first")
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return errors.New(strings.TrimSpace(jsonMessage(msg)))
	}
	return nil
}
