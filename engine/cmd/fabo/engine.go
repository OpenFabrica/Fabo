//go:build linux || darwin

package main

import (
	"cmp"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

// engineCommand handles `fabo engine restart`, which restarts this
// installation's engine on the binary installed now. Replacing the app does
// not restart an engine already running, so it keeps the old version until
// then; the window offers this when the versions differ.
func engineCommand(args []string) error {
	if len(args) != 1 || args[0] != "restart" {
		return errors.New("usage: fabo engine restart")
	}
	in, err := locate()
	if err != nil {
		return err
	}
	return in.restartEngine()
}

// engineVersion is the Fabo release the engine at client runs, or "" if it
// does not answer or is not a Fabo engine. That is FaboVersion; Version is
// now the Docker Engine version clients gate on, and was Fabo's own only on
// an engine older than FaboVersion, so it is the fallback.
func engineVersion(client *http.Client) string {
	var v struct{ Version, FaboVersion, FaboEngine string }
	if get(client, "/version", &v) != nil || v.FaboEngine != "go" {
		return ""
	}
	return cmp.Or(v.FaboVersion, v.Version)
}

// staleEngine returns the engine's version when it differs from this
// launcher's, as when an update or reinstall replaced the app while the old
// engine kept running. A development launcher (0.0.0-dev) compares nothing.
func staleEngine(client *http.Client) (string, bool) {
	if release == "0.0.0-dev" {
		return "", false
	}
	engine := engineVersion(client)
	return engine, engine != "" && engine != release
}

// warnStaleEngine says on stderr, before a docker command, that the engine
// runs another version, so the command's own output stays clean.
func warnStaleEngine(client *http.Client) {
	if engine, stale := staleEngine(client); stale {
		fmt.Fprintf(os.Stderr, "fabo: this fabo is %s, but the engine runs %s. Restart the engine to use one version: fabo engine restart\n", release, engine)
	}
}

// sleepEngine asks the engine to stop its workloads gracefully and release
// the VM, before a restart. On SIGTERM alone the engine powers the VM off
// without stopping containers one by one. It does not mark them stopped by
// the user, so those with a restart policy start again with the new engine.
func sleepEngine(socket string) error {
	client := socketClient(socket)
	client.Timeout = 3 * time.Minute
	defer client.CloseIdleConnections()
	req, err := http.NewRequest(http.MethodPost, "http://fabo/fabo/sleep", nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode >= 300 {
		return fmt.Errorf("POST /fabo/sleep: %s", resp.Status)
	}
	return nil
}

// waitForRelease waits until the engine at socket answers with this
// launcher's version, or with any version for a development launcher, which
// cannot tell the old engine from the new one by version alone.
func waitForRelease(socket string, timeout time.Duration) error {
	client := socketClient(socket)
	defer client.CloseIdleConnections()
	for deadline := time.Now().Add(timeout); time.Now().Before(deadline); time.Sleep(250 * time.Millisecond) {
		if v := engineVersion(client); v != "" && (release == "0.0.0-dev" || v == release) {
			return nil
		}
	}
	return fmt.Errorf("the engine did not come back as %s", release)
}
