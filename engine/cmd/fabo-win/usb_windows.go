package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/OpenFabrica/Fabo/engine/internal/winhost"
)

// winUSB is usbSystem on Windows.
type winUSB struct{ h *host }

// usbipdPath is where usbipd-win's installer (and winget) puts usbipd.exe.
// Only there: Fabo runs it as administrator, so not from a folder on PATH
// the user can write to.
func usbipdPath() string {
	dir := os.Getenv("ProgramFiles")
	if dir == "" {
		dir = `C:\Program Files`
	}
	return filepath.Join(dir, "usbipd-win", "usbipd.exe")
}

// usbipdInstalled reports whether usbipd.exe is there.
func (winUSB) usbipdInstalled() bool {
	info, err := os.Stat(usbipdPath())
	return err == nil && info.Mode().IsRegular()
}

// usbipd runs usbipd quietly; ctx ending stops it.
func (winUSB) usbipd(ctx context.Context, args ...string) ([]byte, error) {
	stdout, stderr, err := runQuiet(ctx, winhost.Quiet(usbipdPath(), args...))
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			if msg := usbipdMessage(stdout, stderr); msg != "" {
				return nil, errors.New(msg)
			}
		}
		return nil, fmt.Errorf("usbipd %s: %w", args[0], err)
	}
	return stdout, nil
}

// usbipdElevated runs usbipd as administrator: Windows asks first.
func (winUSB) usbipdElevated(args ...string) (uint32, error) {
	code, err := winhost.Elevated(usbipdPath(), args...)
	if errors.Is(err, winhost.ErrDeclined) {
		return 0, errUSBCancelled
	}
	return code, err
}

// engineRunning asks the engine for /_ping, as the watchdog does.
func (w winUSB) engineRunning() bool { return w.h.engineAlive() }

// installUSBIPD installs usbipd-win with winget, which lives under the
// user's WindowsApps as an app execution alias, not in System32.
func (winUSB) installUSBIPD(ctx context.Context) error {
	winget := filepath.Join(os.Getenv("LOCALAPPDATA"), "Microsoft", "WindowsApps", "winget.exe")
	stdout, _, err := runQuiet(ctx, winhost.Quiet(winget, "install", "--exact", "--id", "dorssel.usbipd-win", "--silent",
		"--accept-package-agreements", "--accept-source-agreements"))
	var exit *exec.ExitError
	switch {
	case err == nil:
		return nil
	case errors.As(err, &exit):
		if last := lastLine(stdout); last != "" {
			return fmt.Errorf("usbipd-win was not installed: %s", last)
		}
		return errors.New("usbipd-win was not installed")
	}
	return fmt.Errorf("winget did not start (%v): install usbipd-win from https://github.com/dorssel/usbipd-win", err)
}

// runQuiet runs c, killing it if ctx ends first, and returns its output.
func runQuiet(ctx context.Context, c *exec.Cmd) (stdout, stderr []byte, err error) {
	var out, errOut bytes.Buffer
	c.Stdout, c.Stderr = &out, &errOut
	if err := c.Start(); err != nil {
		return nil, nil, err
	}
	done := make(chan error, 1)
	go func() { done <- c.Wait() }()
	select {
	case err = <-done:
	case <-ctx.Done():
		_ = c.Process.Kill()
		<-done
		err = ctx.Err()
	}
	return out.Bytes(), errOut.Bytes(), err
}

// usbState and usbAction are the control API's USB endpoints.
func (h *host) usbState(ctx context.Context) usbView { return h.usb.state(ctx) }

// usbAction attaches, detaches or installs usbipd-win.
func (h *host) usbAction(ctx context.Context, action, busID string) (usbResult, error) {
	return h.usb.action(ctx, action, busID)
}
