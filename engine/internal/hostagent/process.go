//go:build linux || darwin

// Package hostagent manages the local-process transport used by Kiri on Linux.
package hostagent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/libracoder/kiri/engine/internal/guestwire"
)

// Process owns the child and its private socket directory. Call Close to
// release both. The caller owns control-protocol serialization.
type Process struct {
	dir   string
	cmd   *exec.Cmd
	done  chan struct{}
	err   error
	Conn  *net.UnixConn
	Hello guestwire.Hello
}

// Start starts an agent in a private directory under /run. ctx bounds startup
// only. Extra environment entries override inherited ones. A successful
// process continues until Close or until the child exits.
func Start(ctx context.Context, binary string, console io.Writer, env ...string) (*Process, error) {
	cmd := exec.Command(binary)
	cmd.Env = append(cmd.Environ(), env...)
	return start(ctx, "/run", cmd, console)
}

func start(ctx context.Context, parent string, cmd *exec.Cmd, console io.Writer) (*Process, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	dir, err := os.MkdirTemp(parent, "kiri-agent-")
	if err != nil {
		return nil, err
	}
	started := false
	defer func() {
		if !started {
			_ = os.RemoveAll(dir)
		}
	}()
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socket(dir, 'h', guestwire.Port), Net: "unix"})
	if err != nil {
		return nil, err
	}
	defer listener.Close()

	cmd.Env = agentEnv(cmd.Environ(), dir)
	cmd.Stdout, cmd.Stderr = console, console
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start Kiri agent: %w", err)
	}
	p := &Process{dir: dir, cmd: cmd, done: make(chan struct{})}
	go func() {
		p.err = cmd.Wait()
		close(p.done)
	}()
	// Closing the listener unblocks accept if the child fails before dialing.
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-p.done:
		case <-ctx.Done():
		case <-stop:
			return
		}
		_ = listener.Close()
	}()

	p.Conn, err = listener.AcceptUnix()
	if err == nil {
		unblock := context.AfterFunc(ctx, func() { _ = p.Conn.Close() })
		p.Hello, err = guestwire.ReadHello(p.Conn)
		unblock()
	}
	if err == nil {
		err = ctx.Err()
	}
	if err != nil {
		cause := ctx.Err()
		select {
		case <-p.done:
			if cause == nil {
				cause = p.err
				if cause == nil {
					cause = errors.New("agent exited before startup completed")
				}
			}
		default:
		}
		_ = p.Close()
		return nil, fmt.Errorf("Kiri agent startup: %w", errors.Join(cause, err))
	}
	started = true
	return p, nil
}

func agentEnv(env []string, dir string) []string {
	clean := make([]string, 0, len(env)+1)
	for _, entry := range env {
		if !hasEnvKey(entry, "KIRI_AGENT_DIR") && !hasEnvKey(entry, "KIRI_SOCKET") {
			clean = append(clean, entry)
		}
	}
	return append(clean, "KIRI_AGENT_DIR="+dir)
}

func hasEnvKey(entry, key string) bool {
	return len(entry) > len(key) && entry[:len(key)] == key && entry[len(key)] == '='
}

func socket(dir string, side byte, port uint32) string {
	return filepath.Join(dir, fmt.Sprintf("%c%d.sock", side, port))
}

// Done closes when the agent process has exited and been reaped.
func (p *Process) Done() <-chan struct{} { return p.done }

// Wait returns the agent's exit status. It is safe to call alongside Close.
func (p *Process) Wait() error {
	<-p.done
	return p.err
}

// Listen opens a host-side auxiliary port, such as the guest DNS relay.
// The caller closes the listener before closing the process.
func (p *Process) Listen(port uint32) (*net.UnixListener, error) {
	if port == guestwire.Port {
		return nil, fmt.Errorf("port %d is reserved for agent control", port)
	}
	return net.ListenUnix("unix", &net.UnixAddr{Name: socket(p.dir, 'h', port), Net: "unix"})
}

// Dial connects to an agent port, allowing time for its listener to appear.
func (p *Process) Dial(ctx context.Context, port uint32) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	dialer := net.Dialer{}
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		conn, err := dialer.DialContext(ctx, "unix", socket(p.dir, 'g', port))
		if err == nil {
			return conn, nil
		}
		if !errors.Is(err, os.ErrNotExist) && !errors.Is(err, syscall.ECONNREFUSED) {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("dial Kiri agent port %d: %w", port, ctx.Err())
		case <-p.done:
			return nil, fmt.Errorf("Kiri agent exited: %w", err)
		case <-ticker.C:
		}
	}
}

// Close closes the control connection, then gives the agent time to clean up
// before killing it. The direct child is always reaped before returning.
// Calls to Close must be serialized by the owner.
func (p *Process) Close() error {
	if p.Conn != nil {
		_ = p.Conn.Close()
	}
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	select {
	case <-p.done:
	case <-timer.C:
		_ = p.cmd.Process.Kill()
		<-p.done
	}
	return os.RemoveAll(p.dir)
}
