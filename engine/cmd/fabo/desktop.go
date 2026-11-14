//go:build linux || darwin

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"syscall"
	"time"
)

// openDesktop makes sure this installation's Go engine is serving, then
// replaces this process with the desktop window.
func openDesktop(args []string) error {
	in, err := locate()
	if err != nil {
		return err
	}
	desktop := filepath.Join(in.dir, "fabo-desktop")
	if _, err := os.Stat(desktop); err != nil && runtime.GOOS == "linux" {
		desktop = "/usr/lib/fabo/fabo-desktop"
	}
	if info, err := os.Stat(desktop); err != nil || !info.Mode().IsRegular() {
		return errors.New("this installation has no desktop window (fabo-desktop)")
	}
	if err := in.prepareDesktop(args); err != nil {
		return err
	}
	socket := in.socket()
	if err := in.ensureDaemon(socket); err != nil {
		return err
	}
	in.finishDesktop()
	if len(args) > 0 {
		args = args[1:] // "ui"
	}
	return syscall.Exec(desktop, append([]string{desktop}, args...), append(os.Environ(), "FABO_SOCKET="+socket))
}

// socketClient speaks HTTP to the daemon on its Unix socket.
func socketClient(socket string) *http.Client {
	return &http.Client{Timeout: 2 * time.Second, Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socket)
		},
	}}
}

// ensureDaemon starts the engine when nothing answers on the socket, and
// checks that what answers is the Go engine.
func (i installation) ensureDaemon(socket string) error {
	client := socketClient(socket)
	defer client.CloseIdleConnections()
	if !alive(client) {
		if err := i.startDaemon(client); err != nil {
			return err
		}
	}
	var version struct{ FaboEngine string }
	if err := get(client, "/version", &version); err != nil {
		return err
	}
	if version.FaboEngine != "go" {
		return fmt.Errorf("%s is served by another engine, not this installation's", socket)
	}
	return nil
}

// logPath is where the engine's output goes when it runs in the background.
func (i installation) logPath() string { return filepath.Join(i.root(), "daemon.log") }

// spawnDaemon runs the engine in its own session, so closing the window
// leaves containers running, and waits for it to answer.
func (i installation) spawnDaemon(client *http.Client) error {
	engine, args, err := i.daemonCommand()
	if err != nil {
		return err
	}
	log, err := os.OpenFile(i.logPath(), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer log.Close()
	cmd := exec.Command(engine, args...)
	cmd.Stdout, cmd.Stderr = log, log
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	return waitAlive(client, exited, i.logPath())
}

// waitAlive waits for a starting engine to answer. Booting the VM and
// replaying state takes a few seconds on first start. exited, when not nil,
// reports the engine process ending first.
func waitAlive(client *http.Client, exited <-chan error, logPath string) error {
	for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); {
		select {
		case err := <-exited:
			// Another engine may have taken the socket first and be serving.
			if alive(client) {
				return nil
			}
			return fmt.Errorf("the engine stopped while starting (%v); see %s", err, logPath)
		case <-time.After(50 * time.Millisecond):
		}
		if alive(client) {
			return nil
		}
	}
	return fmt.Errorf("the engine did not start; see %s", logPath)
}

// alive reports whether a daemon answers Docker's ping without waking the VM.
func alive(client *http.Client) bool {
	return get(client, "/_ping", nil) == nil
}

// get performs a background GET, which does not reset the VM's idle timer,
// and decodes a JSON reply into out when out is not nil.
func get(client *http.Client, path string, out any) error {
	req, err := http.NewRequest(http.MethodGet, "http://fabo"+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("X-Fabo-Background", "1")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", path, resp.Status)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
