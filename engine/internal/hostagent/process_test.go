//go:build linux || darwin

package hostagent

import (
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/OpenFabrica/FabriBay/engine/internal/guestwire"
)

func TestStartAndClose(t *testing.T) {
	parent := shortTemp(t)
	p, err := start(t.Context(), parent, peer("serve"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })
	if p.Hello.ReadyNS != 20 {
		t.Fatalf("unexpected handshake: %+v", p.Hello)
	}
	conn, err := p.Dial(t.Context(), 1028)
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
	incoming, err := p.Listen(1037)
	if err != nil {
		t.Fatal(err)
	}
	_ = incoming.Close()
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	if err := p.Wait(); err != nil {
		t.Fatalf("agent did not exit cleanly: %v", err)
	}
	requireEmpty(t, parent)
}

func TestStartFailureRemovesPrivateDirectory(t *testing.T) {
	parent := shortTemp(t)
	_, err := start(t.Context(), parent, peer("fail"), nil)
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 23 {
		t.Fatalf("expected child exit status, got %v", err)
	}
	requireEmpty(t, parent)
}

func TestCancelDuringHello(t *testing.T) {
	parent := shortTemp(t)
	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()
	_, err := start(ctx, parent, peer("silent"), nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected deadline, got %v", err)
	}
	requireEmpty(t, parent)
}

func shortTemp(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "fabo-agent-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func requireEmpty(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("expected empty directory, got %v, %v", entries, err)
	}
}

func peer(mode string) *exec.Cmd {
	cmd := exec.Command(os.Args[0], "-test.run=^TestAgentPeer$")
	cmd.Env = append(os.Environ(), "FABO_TEST_AGENT="+mode, "FABO_SOCKET=unused")
	return cmd
}

func TestAgentPeer(t *testing.T) {
	mode := os.Getenv("FABO_TEST_AGENT")
	if mode == "" {
		return
	}
	if mode == "fail" {
		os.Exit(23)
	}
	dir := os.Getenv("FABO_AGENT_DIR")
	if os.Getenv("FABO_SOCKET") != "" {
		t.Fatal("agent inherited daemon socket")
	}
	if info, err := os.Stat(dir); err != nil || info.Mode().Perm() != 0700 {
		t.Fatal("agent directory is not private")
	}
	path := filepath.Join(dir, "g1028.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	conn, err := net.Dial("unix", socket(dir, 'h', guestwire.Port))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if mode == "serve" {
		hello := guestwire.Hello{Version: guestwire.Version, ReadyNS: 20}
		if err := guestwire.WriteFrame(conn, hello.Frame()); err != nil {
			t.Fatal(err)
		}
	}
	var b [1]byte
	_, _ = conn.Read(b[:])
}
