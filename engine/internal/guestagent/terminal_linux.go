package guestagent

import (
	"errors"
	"fmt"
	"io"
	"os"

	"golang.org/x/sys/unix"
)

func mountTerminalDevices() error {
	if err := os.Mkdir("/dev/pts", 0755); err != nil {
		return err
	}
	if err := unix.Mount("devpts", "/dev/pts", "devpts", unix.MS_NOSUID|unix.MS_NOEXEC, "newinstance,ptmxmode=0666,mode=0620,gid=5"); err != nil {
		return err
	}
	if err := os.Symlink("pts/ptmx", "/dev/ptmx"); err != nil {
		return err
	}
	if err := unix.Mknod("/dev/tty", unix.S_IFCHR|0666, int(unix.Mkdev(5, 0))); err != nil {
		return err
	}
	return os.Chmod("/dev/tty", 0666)
}

// attachTerminal runs inside the container's mount namespace. The agent
// receives only the master; the slave is opened in this private devpts.
func attachTerminal(uid int, console bool) error {
	const socket = 4 // extra descriptor following the status pipe
	defer unix.Close(socket)
	master, err := unix.Open("/dev/ptmx", unix.O_RDWR|unix.O_NOCTTY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(master)
	if err := unix.IoctlSetPointerInt(master, unix.TIOCSPTLCK, 0); err != nil {
		return err
	}
	number, err := unix.IoctlGetInt(master, unix.TIOCGPTN)
	if err != nil {
		return err
	}
	device := fmt.Sprintf("/dev/pts/%d", number)
	slave, err := unix.Open(device, unix.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(slave)
	if err := unix.Fchown(slave, uid, -1); err != nil {
		return err
	}
	if console {
		placeholder, err := os.OpenFile("/dev/console", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		if err := placeholder.Close(); err != nil {
			return err
		}
		if err := unix.Mount(device, "/dev/console", "", unix.MS_BIND, ""); err != nil {
			return err
		}
	}
	if err := unix.IoctlSetInt(slave, unix.TIOCSCTTY, 0); err != nil {
		return err
	}
	for fd := range 3 {
		if err := unix.Dup3(slave, fd, 0); err != nil {
			return err
		}
	}
	return unix.Sendmsg(socket, []byte{0}, unix.UnixRights(master), nil, 0)
}

func receiveTerminal(socket int) (*os.File, error) {
	var data [1]byte
	oob := make([]byte, unix.CmsgSpace(4))
	for {
		n, size, flags, _, err := unix.Recvmsg(socket, data[:], oob, unix.MSG_CMSG_CLOEXEC)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return nil, err
		}
		messages, err := unix.ParseSocketControlMessage(oob[:size])
		if err != nil {
			return nil, err
		}
		var fds []int
		for _, msg := range messages {
			rights, err := unix.ParseUnixRights(&msg)
			if err == nil {
				fds = append(fds, rights...)
			}
		}
		if n == 1 && flags&unix.MSG_CTRUNC == 0 && len(fds) == 1 {
			return os.NewFile(uintptr(fds[0]), "container-pty"), nil
		}
		for _, fd := range fds {
			_ = unix.Close(fd)
		}
		return nil, fmt.Errorf("expected one terminal descriptor")
	}
}

type terminalOutput struct{ file *os.File }

func (t terminalOutput) Read(buf []byte) (int, error) {
	n, err := t.file.Read(buf)
	if errors.Is(err, unix.EIO) { // Linux reports slave hangup as EIO.
		return n, io.EOF
	}
	return n, err
}

func (r *Runtime) resize(id string, rows, cols uint16) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if child := r.children[id]; child != nil && child.terminal != nil {
		if conn, err := child.terminal.SyscallConn(); err == nil {
			_ = conn.Control(func(fd uintptr) {
				_ = unix.IoctlSetWinsize(int(fd), unix.TIOCSWINSZ, &unix.Winsize{Row: rows, Col: cols})
			})
		}
	}
}
