//go:build darwin && cgo

package usbhost

import (
	"context"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestHelperRejectsWrongUserAndNonSocket(t *testing.T) {
	for _, wrongUser := range []bool{true, false} {
		if wrongUser && os.Getuid() == 0 {
			continue
		}
		l, path := unixListener(t)
		done := make(chan error, 1)
		go func() {
			c, e := l.AcceptUnix()
			if e != nil {
				done <- e
				return
			}
			defer c.Close()
			uid := uint32(os.Getuid())
			if wrongUser {
				uid++
			}
			done <- helperRequest(context.Background(), c, uid, "/must-not-execute")
		}()
		c, e := net.DialUnix("unix", nil, &net.UnixAddr{Name: path, Net: "unix"})
		if e != nil {
			t.Fatal(e)
		}
		if !wrongUser {
			f, e := os.CreateTemp(t.TempDir(), "regular")
			if e != nil {
				t.Fatal(e)
			}
			_, _, e = c.WriteMsgUnix([]byte("attach 0x1234\n"), unix.UnixRights(int(f.Fd())), nil)
			f.Close()
			if e != nil {
				t.Fatal(e)
			}
		}
		select {
		case e := <-done:
			want := "not your USB helper"
			if !wrongUser {
				want = "not a socket"
			}
			if e == nil || !strings.Contains(e.Error(), want) {
				t.Fatal(e)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("helper request hung")
		}
		c.Close()
	}
}
