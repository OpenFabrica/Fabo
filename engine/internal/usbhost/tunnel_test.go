//go:build darwin || linux

package usbhost

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func unixListener(t *testing.T) (*net.UnixListener, string) {
	t.Helper()
	dir, e := os.MkdirTemp("/tmp", "usb-host-")
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	path := filepath.Join(dir, "socket")
	l, e := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { l.Close() })
	return l, path
}
func TestUpgradeDoesNotConsumeFirstURB(t *testing.T) {
	l, path := unixListener(t)
	done := make(chan error, 1)
	go func() {
		c, e := l.AcceptUnix()
		if e != nil {
			done <- e
			return
		}
		defer c.Close()
		req, e := http.ReadRequest(bufio.NewReader(c))
		if e != nil {
			done <- e
			return
		}
		if req.URL.Query().Get("location") != "test" {
			t.Error(req.URL)
		}
		_, e = c.Write(append([]byte("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n"), []byte("first-urb")...))
		done <- e
	}()
	c, e := Connect(path, Device{Location: "test", Name: "test serial", Speed: 1})
	if e != nil {
		t.Fatal(e)
	}
	defer c.Close()
	c.SetReadDeadline(time.Now().Add(time.Second))
	p := make([]byte, 9)
	if _, e = io.ReadFull(c, p); e != nil || string(p) != "first-urb" {
		t.Fatal(string(p), e)
	}
	if e = <-done; e != nil {
		t.Fatal(e)
	}
}
