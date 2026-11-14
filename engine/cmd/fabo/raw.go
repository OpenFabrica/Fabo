//go:build linux || darwin

package main

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
)

// Raw commands own a guest and an exclusive store, never the daemon socket.
func (i installation) rawLaunch(args []string) (string, []string, error) {
	engine, e := i.engine()
	if e != nil {
		return "", nil, e
	}
	root := filepath.Join(i.root(), "raw")
	launch := []string{"raw", "-state", root}
	if runtime.GOOS == "darwin" {
		guest := os.Getenv("FABO_GUEST_DIR")
		if guest == "" {
			guest = filepath.Join(i.dir, "..", "Resources", "guest")
		}
		kernel := "Image"
		if runtime.GOARCH == "amd64" {
			kernel = "bzImage"
		}
		launch = append(launch, "-kernel", filepath.Join(guest, kernel), "-initrd", filepath.Join(guest, "initramfs-go.cpio"), "-disk", filepath.Join(root, "data.img"), "-disk-template", filepath.Join(guest, "data.img.tgz"), "-cpus", "2", "-memory", "2048")
	} else {
		agent := os.Getenv("FABO_AGENT")
		if agent == "" {
			agent = filepath.Join(filepath.Dir(engine), "fabo-init")
		}
		launch = append(launch, "-agent", agent)
	}
	return engine, append(append(launch, "--"), args...), nil
}
func rawCommand(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: fabo raw run [OPTIONS] IMAGE [COMMAND...] | pull IMAGE | images | rmi IMAGE...")
	}
	switch args[0] {
	case "run", "pull", "images", "rmi":
	default:
		return errors.New("unknown raw command: " + args[0])
	}
	in, e := locate()
	if e != nil {
		return e
	}
	engine, launch, e := in.rawLaunch(args)
	if e != nil {
		return e
	}
	return syscall.Exec(engine, append([]string{engine}, launch...), os.Environ())
}
