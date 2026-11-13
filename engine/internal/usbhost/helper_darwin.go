//go:build darwin && cgo

package usbhost

import (
	"context"
	"errors"
	"fmt"
	"golang.org/x/sys/unix"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

func validLocation(s string) (uint32, error) {
	if !strings.HasPrefix(s, "0x") || len(s) < 3 || len(s) > 10 {
		return 0, errors.New("invalid USB location")
	}
	v, e := strconv.ParseUint(s[2:], 16, 32)
	return uint32(v), e
}
func Helper(ctx context.Context, label string, uid uint32) error {
	if os.Geteuid() != 0 {
		return errors.New("the USB helper runs as root, under launchd")
	}
	if !strings.HasPrefix(label, "dev.kiri.usb") || strings.ContainsFunc(label, func(c rune) bool { return !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '.' || c == '-') }) {
		return errors.New("invalid USB helper label")
	}
	path := "/var/run/" + label + ".sock"
	if e := os.Remove(path); e != nil && !os.IsNotExist(e) {
		return e
	}
	listener, e := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if e != nil {
		return e
	}
	defer listener.Close()
	if e = os.Chmod(path, 0666); e != nil {
		return e
	}
	exe, e := os.Executable()
	if e != nil {
		return e
	}
	stop := context.AfterFunc(ctx, func() { listener.Close() })
	defer stop()
	slots := make(chan struct{}, 32)
	var children sync.WaitGroup
	defer children.Wait()
	for {
		conn, e := listener.AcceptUnix()
		if e != nil {
			if ctx.Err() != nil {
				return nil
			}
			return e
		}
		select {
		case slots <- struct{}{}:
		default:
			fmt.Fprintln(conn, "error: too many devices attached")
			conn.Close()
			continue
		}
		children.Add(1)
		go func() {
			defer children.Done()
			defer func() { <-slots }()
			defer conn.Close()
			if e := helperRequest(ctx, conn, uid, exe); e != nil {
				fmt.Fprintf(conn, "error: %v\n", e)
			}
		}()
	}
}
func helperRequest(ctx context.Context, conn *net.UnixConn, uid uint32, exe string) error {
	raw, e := conn.SyscallConn()
	if e != nil {
		return e
	}
	var cred *unix.Xucred
	var credErr error
	if e = raw.Control(func(fd uintptr) { cred, credErr = unix.GetsockoptXucred(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERCRED) }); e != nil {
		return e
	}
	if credErr != nil {
		return credErr
	}
	if cred.Uid != uid && cred.Uid != 0 {
		return errors.New("not your USB helper")
	}
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	data, control := make([]byte, 256), make([]byte, unix.CmsgSpace(4*8))
	n, oob, flags, _, e := conn.ReadMsgUnix(data, control)
	if e != nil {
		return e
	}
	messages, e := unix.ParseSocketControlMessage(control[:oob])
	if e != nil {
		return e
	}
	fds := []int{}
	for _, m := range messages {
		rights, e := unix.ParseUnixRights(&m)
		if e != nil {
			for _, fd := range fds {
				unix.Close(fd)
			}
			return e
		}
		fds = append(fds, rights...)
	}
	defer func() {
		for _, fd := range fds {
			unix.Close(fd)
		}
	}()
	if flags&(unix.MSG_TRUNC|unix.MSG_CTRUNC) != 0 || len(fds) != 1 {
		return errors.New("expected one connection descriptor")
	}
	line := string(data[:n])
	for !strings.Contains(line, "\n") && len(line) < 256 {
		b := make([]byte, 1)
		if _, e = conn.Read(b); e != nil {
			return e
		}
		line += string(b)
	}
	location, ok := strings.CutPrefix(strings.TrimSuffix(line, "\n"), "attach ")
	if !ok {
		return errors.New("unknown helper request")
	}
	if _, e = validLocation(location); e != nil {
		return e
	}
	var stat unix.Stat_t
	if e = unix.Fstat(fds[0], &stat); e != nil {
		return e
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFSOCK {
		return errors.New("the connection is not a socket")
	}
	unix.CloseOnExec(fds[0])
	stream := os.NewFile(uintptr(fds[0]), "usb-stream")
	fds = nil
	defer stream.Close()
	status, e := conn.File()
	if e != nil {
		return e
	}
	defer status.Close()
	cmd := exec.CommandContext(ctx, exe, "usb", "serve", location, "--fd", "3", "--status", "4")
	cmd.ExtraFiles = []*os.File{stream, status}
	cmd.Env = []string{"HOME=/var/root", "PATH=/usr/bin:/bin:/usr/sbin:/sbin"}
	cmd.Stderr = os.Stderr
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 5 * time.Second
	if e = cmd.Start(); e != nil {
		return e
	}
	_ = conn.SetDeadline(time.Time{})
	return cmd.Wait()
}
func ServeCaptured(ctx context.Context, location string, fd, statusFD int) error {
	if os.Geteuid() != 0 {
		return errors.New("USB capture needs root (the USB helper runs it)")
	}
	loc, e := validLocation(location)
	if e != nil {
		return e
	}
	if fd <= 2 || statusFD <= 2 || fd == statusFD {
		return errors.New("--fd and --status are required")
	}
	file := os.NewFile(uintptr(fd), "usb-stream")
	conn, e := net.FileConn(file)
	if e != nil {
		file.Close()
		return e
	}
	defer conn.Close()
	defer file.Close()
	status := os.NewFile(uintptr(statusFD), "usb-status")
	defer status.Close()
	device, e := Capture(loc, fd)
	if e != nil {
		fmt.Fprintf(status, "error: %v\n", e)
		return e
	}
	defer device.Dispose()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	if _, e = fmt.Fprintln(status, "ok"); e != nil {
		device.Close()
		return e
	}
	status.Close()
	e = Serve(device, conn)
	if ctx.Err() != nil {
		return nil
	}
	return e
}
