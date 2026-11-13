//go:build cgo

package main

/*
#include <sys/types.h>
#include <unistd.h>
*/
import "C"

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/libracoder/kiri/engine/internal/finder"
	"github.com/libracoder/kiri/engine/internal/guestwire"
	"github.com/libracoder/kiri/engine/internal/localengine"
)

func (o guestOptions) startFinder(s *supervisor) func() {
	stop, err := startFinder(s, o.finderSocket, o.finderMount)
	if err != nil {
		log.Printf("container files in Finder: %v", err)
		return func() {}
	}
	return stop
}

func startFinder(source finder.Source, socket, mount string) (func(), error) {
	if socket == "" && mount == "" {
		return func() {}, nil
	}
	if !filepath.IsAbs(socket) || !filepath.IsAbs(mount) || strings.Contains(socket, ",") {
		return nil, fmt.Errorf("invalid Finder socket or mount path")
	}
	dir := filepath.Dir(socket)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return nil, err
	}
	owner, ok := info.Sys().(*syscall.Stat_t)
	if !info.IsDir() || !ok || owner.Uid != uint32(os.Geteuid()) {
		return nil, fmt.Errorf("Finder socket directory must be owned by the daemon user")
	}
	if err := os.Chmod(dir, 0700); err != nil {
		return nil, err
	}
	listener, err := listenSocket(socket)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(socket, 0600); err != nil {
		_ = listener.Close()
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		if err := finder.Serve(ctx, listener, source, finderPeerAllowed); err != nil {
			log.Printf("Finder file service: %v", err)
		}
	}()
	if _, disabled := os.LookupEnv("KIRI_NO_FINDER"); !disabled {
		if err := mountFinder(socket, mount); err != nil {
			log.Printf("Finder mount at %s: %v", mount, err)
		}
	}
	return func() {
		if err := unmountFinder(socket, mount); err != nil {
			log.Printf("unmount Finder at %s: %v", mount, err)
		}
		cancel()
		_ = listener.Close()
	}, nil
}

func finderPeerAllowed(conn *net.UnixConn) bool {
	raw, err := conn.SyscallConn()
	if err != nil {
		return false
	}
	allowed := false
	err = raw.Control(func(fd uintptr) {
		var uid C.uid_t
		var gid C.gid_t
		allowed = C.getpeereid(C.int(fd), &uid, &gid) == 0 && (uid == 0 || uint32(uid) == uint32(os.Geteuid()))
	})
	return err == nil && allowed
}

func mountFinder(socket, dir string) error {
	current, err := mountAt(dir)
	if err != nil {
		return err
	}
	if current != nil {
		if !finderMountOwned(current, socket) {
			return fmt.Errorf("another filesystem is mounted here")
		}
		if err := unmountFinder(socket, dir); err != nil {
			return err
		}
		if current, err := mountAt(dir); err != nil {
			return err
		} else if current != nil {
			return fmt.Errorf("the previous Finder mount is still present")
		}
	}
	info, err := os.Lstat(dir)
	switch {
	case errors.Is(err, os.ErrNotExist):
		if err := os.MkdirAll(dir, 0755); err != nil {
			return err
		}
	case err != nil:
		return err
	case !info.IsDir():
		return fmt.Errorf("mount path is not a directory")
	default:
		entries, err := os.ReadDir(dir)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if entry.Name() != ".DS_Store" && entry.Name() != ".localized" {
				return fmt.Errorf("mount directory already contains files")
			}
		}
	}
	options := "vers=3,proto=ticotsord,port=" + socket + ",mountport=" + socket + ",rdonly,nolocks,soft,intr,timeo=10,retrans=2,noac"
	return runMountCommand("/sbin/mount_nfs", "-o", options, "<>:/", dir)
}

func unmountFinder(socket, dir string) error {
	mount, err := mountAt(dir)
	if err != nil || !finderMountOwned(mount, socket) {
		return err
	}
	return runMountCommand("/sbin/umount", "-f", dir)
}

func finderMountOwned(mount *unix.Statfs_t, socket string) bool {
	return mount != nil && unix.ByteSliceToString(mount.Fstypename[:]) == "nfs" &&
		unix.ByteSliceToString(mount.Mntfromname[:]) == "<"+socket+">:/"
}

func mountAt(dir string) (*unix.Statfs_t, error) {
	real := dir
	if parent, err := filepath.EvalSymlinks(filepath.Dir(dir)); err == nil {
		real = filepath.Join(parent, filepath.Base(dir))
	}
	n, err := unix.Getfsstat(nil, unix.MNT_NOWAIT)
	if err != nil {
		return nil, err
	}
	for range 4 {
		mounts := make([]unix.Statfs_t, n+8)
		n, err = unix.Getfsstat(mounts, unix.MNT_NOWAIT)
		if err != nil {
			return nil, err
		}
		for i := range min(n, len(mounts)) {
			name := filepath.Clean(unix.ByteSliceToString(mounts[i].Mntonname[:]))
			if strings.EqualFold(name, dir) || strings.EqualFold(name, real) {
				return &mounts[i], nil
			}
		}
		if n < len(mounts) {
			return nil, nil
		}
	}
	return nil, fmt.Errorf("mount table changed while checking Finder")
}

func runMountCommand(command string, args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, command, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %w: %s", filepath.Base(command), err, strings.TrimSpace(string(output)))
	}
	return nil
}

func (s *supervisor) FileContainers() map[string]string {
	s.mu.Lock()
	if !s.isActiveLocked() {
		s.mu.Unlock()
		return nil
	}
	engine := s.current.engine
	s.mu.Unlock()
	return engine.FileContainers()
}

// Keep idle shutdown from retiring a guest during a file request. Finder
// metadata requests do not wake a sleeping VM.
func (s *supervisor) finderEngine() (*localengine.Engine, func(), error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.isActiveLocked() {
		return nil, nil, syscall.ESTALE
	}
	current := s.current
	current.requests.Add(1)
	current.inFlight++
	return current.engine, func() {
		s.mu.Lock()
		current.inFlight--
		s.mu.Unlock()
		current.requests.Done()
	}, nil
}

func (s *supervisor) FileStat(ctx context.Context, id, path string) (guestwire.FileInfo, error) {
	engine, done, err := s.finderEngine()
	if err != nil {
		return guestwire.FileInfo{}, err
	}
	defer done()
	return engine.FileStat(ctx, id, path)
}

func (s *supervisor) FileList(ctx context.Context, id, path string) ([]guestwire.FileInfo, error) {
	engine, done, err := s.finderEngine()
	if err != nil {
		return nil, err
	}
	defer done()
	return engine.FileList(ctx, id, path)
}

func (s *supervisor) FileRead(ctx context.Context, query guestwire.FileQuery) ([]byte, error) {
	engine, done, err := s.finderEngine()
	if err != nil {
		return nil, err
	}
	defer done()
	return engine.FileRead(ctx, query)
}
