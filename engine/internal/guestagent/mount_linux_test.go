package guestagent

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

// A mount point directory the engine makes for a copy-up volume is marked,
// so a later start does not copy its root-owned 0755 over the volume; an
// existing directory, possibly the image's, is never marked.
func TestOpenTargetMarksOnlyDirectoriesItMakes(t *testing.T) {
	dir := t.TempDir()
	probe := filepath.Join(dir, "probe")
	if err := os.Mkdir(probe, 0755); err != nil {
		t.Fatal(err)
	}
	if err := unix.Setxattr(probe, mountpointMark, []byte("1"), 0); errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.EPERM) {
		t.Skipf("no user xattrs here: %v", err)
	} else if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "image"), 0755); err != nil {
		t.Fatal(err)
	}
	root, err := unix.Open(dir, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(root)
	for _, tt := range []struct {
		name       string
		mark, want bool
	}{
		{"made", true, true},
		{"image", true, false},
		{"unmarked", false, false},
	} {
		fd, err := openTarget(root, root, tt.name, tt.name, true, false, tt.mark)
		if err != nil {
			t.Fatalf("%s: %v", tt.name, err)
		}
		got := engineMountpoint(fd)
		_ = unix.Close(fd)
		if got != tt.want {
			t.Errorf("%s: marked %v, want %v", tt.name, got, tt.want)
		}
	}
}

// A restart finds the marked mount point the first start made, and leaves
// an empty volume's own owner and mode alone (a tmpfs volume's uid=, gid=
// and mode=); an unmarked image directory still gives the volume its
// metadata.
func TestPopulateVolumesSkipsEngineMountpoint(t *testing.T) {
	image := t.TempDir()
	for _, name := range []string{"made", "image"} {
		if err := os.Mkdir(filepath.Join(image, name), 0755); err != nil {
			t.Fatal(err)
		}
	}
	if err := unix.Setxattr(filepath.Join(image, "made"), mountpointMark, []byte("1"), 0); errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.EPERM) {
		t.Skipf("no user xattrs here: %v", err)
	} else if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		target string
		want   os.FileMode
	}{
		{"/made", 0750 | os.ModeSetgid},
		{"/image", 0755},
	} {
		volume := t.TempDir()
		if err := os.Chmod(volume, 0750|os.ModeSetgid); err != nil {
			t.Fatal(err)
		}
		fd, err := unix.Open(volume, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
		if err != nil {
			t.Fatal(err)
		}
		err = populateVolumes(image, []detachedMount{{target: tt.target, dir: true, fd: -1, copyFD: fd}})
		_ = unix.Close(fd)
		if err != nil {
			t.Fatalf("%s: %v", tt.target, err)
		}
		info, err := os.Stat(volume)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode() & (os.ModePerm | os.ModeSetgid); got != tt.want {
			t.Errorf("%s: volume mode %v, want %v", tt.target, got, tt.want)
		}
	}
}
