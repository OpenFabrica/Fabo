package guestagent

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/OpenFabrica/Fabo/engine/internal/guestwire"
)

// testCgroup creates an empty cgroup under the unified hierarchy, or skips
// where the test cannot create one.
func testCgroup(t *testing.T, name string) *os.File {
	t.Helper()
	dir := filepath.Join("/sys/fs/cgroup", fmt.Sprintf("fabo-test-%d-%s", os.Getpid(), name))
	if err := os.Mkdir(dir, 0o755); err != nil {
		if errors.Is(err, unix.EROFS) || errors.Is(err, unix.EACCES) || errors.Is(err, unix.EPERM) || errors.Is(err, os.ErrNotExist) {
			t.Skipf("cannot create a cgroup: %v", err)
		}
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(dir) })
	f, err := os.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f
}

// Containers without device rules share one loaded filter; rules of their
// own still get a program of their own, which is not kept.
func TestDefaultDeviceFilterLoadedOnce(t *testing.T) {
	first, second := testCgroup(t, "a"), testCgroup(t, "b")
	if err := restrictDevices(first, nil); err != nil {
		if errors.Is(err, unix.EPERM) || errors.Is(err, unix.EINVAL) {
			t.Skipf("cgroup device filters unavailable: %v", err)
		}
		t.Fatal(err)
	}
	loaded := defaultDeviceFilter.fd
	if loaded <= 0 {
		t.Fatalf("default filter not kept: fd %d", loaded)
	}
	if err := restrictDevices(second, nil); err != nil {
		t.Fatal(err)
	}
	if defaultDeviceFilter.fd != loaded {
		t.Fatalf("default filter reloaded: fd %d, then %d", loaded, defaultDeviceFilter.fd)
	}
	if flags, err := unix.FcntlInt(uintptr(loaded), unix.F_GETFD, 0); err != nil || flags&unix.FD_CLOEXEC == 0 {
		t.Fatalf("default filter descriptor leaks into children: flags %d, %v", flags, err)
	}
	custom := testCgroup(t, "c")
	rules := []guestwire.DeviceRule{{Kind: unix.BPF_DEVCG_DEV_CHAR, Major: 10, Minor: 229, Access: unix.BPF_DEVCG_ACC_READ}}
	if err := restrictDevices(custom, rules); err != nil {
		t.Fatal(err)
	}
	if defaultDeviceFilter.fd != loaded {
		t.Fatal("custom rules replaced the default filter")
	}
}
