//go:build linux || (darwin && cgo)

package main

import (
	"net"
	"os"
	"path/filepath"
	"testing"
)

func TestListenSocketRecoversAfterAnUncleanExit(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "kiri-sock-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "docker.sock")
	old, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	old.SetUnlinkOnClose(false)
	if duplicate, err := listenSocket(path); err == nil {
		duplicate.Close()
		old.Close()
		t.Fatal("listen replaced a running daemon's socket")
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := listenSocket(path)
	if err != nil {
		t.Fatal(err)
	}
	restarted.Close()
	if err := os.WriteFile(path, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if duplicate, err := listenSocket(path); err == nil {
		duplicate.Close()
		t.Fatal("listen replaced a regular file")
	}
	if content, err := os.ReadFile(path); err != nil || string(content) != "keep" {
		t.Fatalf("regular file was changed: %q, %v", content, err)
	}
}
