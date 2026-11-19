package guestagent

import (
	"os"
	"os/exec"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

// A child started as launchProcess starts every container and exec leads
// a session and a process group of its own, so a kill(0, ...) inside it
// cannot reach the agent; Setpgid next to Setsid would fail with EPERM.
func TestChildLeadsItsOwnSession(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("no /bin/sh")
	}
	cmd := exec.Command("/bin/sh", "-c", "sleep 30")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	sid, sidErr := unix.Getsid(pid)
	pgid, pgidErr := unix.Getpgid(pid)
	child := &childProcess{Cmd: cmd, signalGroup: true}
	child.signal(syscall.SIGKILL)
	_ = cmd.Wait()
	if sidErr != nil || pgidErr != nil || sid != pid || pgid != pid {
		t.Errorf("child %d: session %d (%v), group %d (%v)", pid, sid, sidErr, pgid, pgidErr)
	}
	if ours, _ := unix.Getpgid(0); ours == pid {
		t.Error("the test shares the child's process group")
	}

	both := exec.Command("/bin/sh", "-c", "exit 0")
	both.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setpgid: true}
	if err := both.Run(); err == nil {
		t.Error("Setsid with Setpgid started; launchProcess no longer needs to clear Setpgid")
	}
}
