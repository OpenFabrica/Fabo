package guestagent

import (
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"golang.org/x/sys/unix"
)

// runInit runs after namespace setup, on the locked thread with the
// container's identity and seccomp policy. ForkExec keeps that policy in
// the workload and reports exec failures before the guest signals readiness.
func runInit(status *os.File, program string, argv, env []string, tty bool) error {
	signals := make(chan os.Signal, 64)
	signal.Notify(signals, initSignals()...)
	defer signal.Stop(signals)
	pid, err := syscall.ForkExec(program, argv, &syscall.ProcAttr{
		Env: env, Files: []uintptr{0, 1, 2},
		Sys: &syscall.SysProcAttr{Foreground: tty, Ctty: 0},
	})
	if err != nil {
		return err
	}
	_ = status.Close()
	for {
		code, exited, err := reapInitChildren(pid)
		if err != nil {
			fmt.Fprintln(os.Stderr, "container init:", err)
			os.Exit(255)
		}
		if exited {
			os.Exit(code)
		}
		sig := (<-signals).(syscall.Signal)
		if sig != unix.SIGCHLD {
			_ = unix.Kill(pid, sig)
		}
	}
}

func initSignals() []os.Signal {
	var result []os.Signal
	for sig := syscall.Signal(1); sig <= 64; sig++ {
		switch sig {
		case unix.SIGKILL, unix.SIGSTOP, unix.SIGILL, unix.SIGTRAP, unix.SIGABRT,
			unix.SIGBUS, unix.SIGFPE, unix.SIGSEGV, unix.SIGSYS, unix.SIGTTIN, unix.SIGTTOU,
			32, 33: // reserved by Linux threading libraries
			continue
		}
		result = append(result, sig)
	}
	return result
}

func reapInitChildren(workload int) (int, bool, error) {
	for {
		var status unix.WaitStatus
		pid, err := unix.Wait4(-1, &status, unix.WNOHANG, nil)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil || pid == 0 {
			return 0, false, err
		}
		if pid == workload {
			if status.Signaled() {
				return 128 + int(status.Signal()), true, nil
			}
			return status.ExitStatus(), true, nil
		}
	}
}
