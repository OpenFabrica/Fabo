//go:build linux || darwin

package hostagent

import (
	"bytes"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/OpenFabrica/FabriBay/engine/internal/guestwire"
)

func TestRustRunInterop(t *testing.T) {
	binary := os.Getenv("FABO_TEST_RUST_PEER")
	if binary == "" {
		t.Skip("set FABO_TEST_RUST_PEER to the compiled Rust peer")
	}
	cmd := exec.Command(binary)
	cmd.Env = append(os.Environ(), "FABO_TEST_RUN=1")
	p, err := start(t.Context(), shortTemp(t), cmd, os.Stderr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })
	if err := p.Conn.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}

	spec := guestwire.RunSpec{
		ID: "interop", Layers: []string{"top", "base"}, Argv: []string{"echo", "hello"},
		Env: []string{"A=1"}, Cwd: "/work", User: "10:20", Hostname: "demo", Flags: 4,
		Network: guestwire.Network{
			Bridge: "bridge", IP: [4]byte{10, 0, 0, 2}, Prefix: 24, Gateway: [4]byte{10, 0, 0, 1},
			ExtraHosts: []string{"test:10.0.0.3"}, DNSNames: []string{"demo"}, Options: []string{"internal=1"},
		},
		Mounts: []string{"/src\t/dst\tro"}, Options: []string{"group\tstaff"}, Caps: 128,
		Limits: guestwire.Limits{MemoryMax: 100, CPUQuota: 200, CPUPeriod: 300, CPUWeight: 400, PIDsMax: 500, ShmSize: 600},
	}
	var stdout, stderr bytes.Buffer
	exited := make(chan guestwire.Exit, 1)
	session := guestwire.NewSession(p.Conn, func(event guestwire.RunEvent) {
		switch event.Kind {
		case guestwire.OutputMessage:
			if event.Stream == 1 {
				stdout.Write(event.Data)
			} else {
				stderr.Write(event.Data)
			}
		case guestwire.ExitedMessage:
			exited <- event.Exit
		}
	})
	defer session.Close()
	const digest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	if have, err := session.HasLayer(t.Context(), digest); err != nil || have {
		t.Fatalf("layer lookup: have=%v err=%v", have, err)
	}
	stored, err := session.SendLayer(t.Context(), digest, "application/vnd.oci.image.layer.v1.tar", bytes.NewBufferString("abc"), 3)
	if err != nil || stored != (guestwire.LayerStored{UnpackedBytes: 8, ElapsedNS: 9}) {
		t.Fatalf("layer transfer: %+v %v", stored, err)
	}
	pid, err := session.Start(t.Context(), spec)
	if err != nil {
		t.Fatal(err)
	}
	var exit guestwire.Exit
	select {
	case exit = <-exited:
	case <-session.Done():
		t.Fatalf("guest disconnected before exit: %v", session.Err())
	}
	if pid != 42 || stdout.String() != "hello\n" || stderr.String() != "err\n" {
		t.Fatalf("pid = %d, stdout = %q, stderr = %q", pid, stdout.String(), stderr.String())
	}
	if exit != (guestwire.Exit{Code: 137, SetupNS: 31, RunNS: 42, OOMKilled: true}) {
		t.Fatalf("unexpected exit: %+v", exit)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	if err := p.Wait(); err != nil {
		t.Fatal(err)
	}
}
