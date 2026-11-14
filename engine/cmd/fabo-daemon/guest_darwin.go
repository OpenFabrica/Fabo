//go:build cgo

package main

/*
#cgo CFLAGS: -x objective-c -fobjc-arc -mmacosx-version-min=14.0
#cgo LDFLAGS: -framework Foundation -framework Virtualization
#include <stdlib.h>
#include "virtualization.h"
*/
import "C"

import (
	"context"
	"crypto/sha256"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"net"
	"os"
	"path/filepath"
	"runtime/cgo"
	"time"
	"unsafe"

	"github.com/OpenFabrica/Fabo/engine/internal/guestwire"
	"github.com/OpenFabrica/Fabo/engine/internal/localengine"
)

type guestOptions struct {
	kernel, initrd, disk, console string
	finderSocket, finderMount     string
	diskTemplate                  string
	shares                        []string
	standardShares                bool
	engineSocket                  string
	cpus                          uint
	memory                        uint64
	offline                       bool
}

func (o *guestOptions) register(flags *flag.FlagSet) {
	flags.StringVar(&o.kernel, "kernel", "", "Fabo Linux kernel image")
	flags.StringVar(&o.initrd, "initrd", "", "Go guest initramfs")
	flags.StringVar(&o.disk, "disk", "", "optional dedicated ext4 VM data image")
	flags.StringVar(&o.diskTemplate, "disk-template", "", "sparse archive for provisioning a new VM data disk")
	flags.StringVar(&o.console, "console", "", "optional VM console log file")
	flags.StringVar(&o.finderSocket, "finder-socket", "", "private Unix socket for Finder's read-only file view")
	flags.StringVar(&o.finderMount, "finder-mount", "", "directory for the Finder mount")
	flags.UintVar(&o.cpus, "cpus", 2, "VM virtual CPU count")
	flags.Uint64Var(&o.memory, "memory", 1024, "VM memory in MiB")
	flags.Func("share", "host directory available for VM bind mounts (repeatable)", func(path string) error {
		if path != "" {
			o.shares = append(o.shares, path)
		}
		return nil
	})
	flags.BoolVar(&o.standardShares, "standard-shares", false, "share the standard Fabo host directories")
	flags.BoolVar(&o.offline, "offline", false, "boot the VM without its NAT interface")
}

func standardMacShares() []string {
	var shares []string
	for _, dir := range []string{"/Users", "/Volumes", "/private", os.Getenv("HOME")} {
		if info, err := os.Stat(dir); err == nil && info.IsDir() {
			shares = append(shares, dir)
		}
	}
	return shares
}

func (o *guestOptions) configureEngineSocket(socket string) bool {
	o.engineSocket = socket
	return false
}

func (o guestOptions) valid() bool {
	return o.kernel != "" && o.initrd != "" && o.cpus > 0 && o.cpus <= math.MaxUint32 && o.memory > 0 && o.memory <= math.MaxUint64>>20
}

func vmError(message *C.char) error {
	if message == nil {
		return errors.New("unknown Virtualization error")
	}
	defer C.free(unsafe.Pointer(message))
	return errors.New(C.GoString(message))
}

func (o guestOptions) start(ctx context.Context, stateDir string) (guestConnection, error) {
	kernel, initrd := C.CString(o.kernel), C.CString(o.initrd)
	disk, console := C.CString(o.disk), C.CString(o.console)
	defer C.free(unsafe.Pointer(kernel))
	defer C.free(unsafe.Pointer(initrd))
	defer C.free(unsafe.Pointer(disk))
	defer C.free(unsafe.Pointer(console))

	var hardwareAddress string
	if !o.offline {
		path, err := filepath.Abs(stateDir)
		if err != nil {
			return guestConnection{}, err
		}
		path, err = filepath.EvalSymlinks(path)
		if err != nil {
			return guestConnection{}, err
		}
		digest := sha256.Sum256([]byte(path))
		hardwareAddress = append(net.HardwareAddr{2}, digest[:5]...).String()
	}
	mac := C.CString(hardwareAddress)
	defer C.free(unsafe.Pointer(mac))

	shares := o.shares
	if o.standardShares {
		shares = append(standardMacShares(), shares...)
	}
	roots, err := localengine.CanonicalShareRoots(shares)
	if err != nil {
		return guestConnection{}, fmt.Errorf("open VM shares: %w", err)
	}
	paths := make([]*C.char, 0, len(roots))
	for _, root := range roots {
		value := C.CString(root)
		defer C.free(unsafe.Pointer(value))
		paths = append(paths, value)
	}
	shareTag := C.CString(guestwire.HostShareTag)
	defer C.free(unsafe.Pointer(shareTag))

	reader, writer, err := os.Pipe()
	if err != nil {
		return guestConnection{}, err
	}
	var message *C.char
	hostCtx, stopHost := context.WithCancel(ctx)
	host := &hostBound{ctx: hostCtx, slots: make(chan struct{}, 128), engineSocket: o.engineSocket}
	handle := cgo.NewHandle(host)
	machine := C.fabo_vm_start(kernel, initrd, disk, console, mac, unsafe.SliceData(paths), C.size_t(len(paths)), shareTag, C.uint32_t(o.cpus), C.uint64_t(o.memory<<20),
		guestwire.Port, guestwire.HostRelayPort, C.uintptr_t(handle), C.int(writer.Fd()), &message)
	_ = writer.Close()
	if machine == nil {
		stopHost()
		host.workers.Wait()
		handle.Delete()
		_ = reader.Close()
		return guestConnection{}, vmError(message)
	}
	dialer := &vmRelayDialer{machine: machine}
	var conn net.Conn
	closeVM := func() error {
		dialer.close()
		stopHost()
		defer reader.Close()
		defer func() {
			C.fabo_vm_release(machine)
			host.workers.Wait()
			handle.Delete()
		}()
		if conn != nil {
			_ = conn.Close()
		}
		C.fabo_vm_disconnect(machine)
		_ = reader.SetReadDeadline(time.Now().Add(5 * time.Second))
		for {
			event, err := readVMEvent(reader)
			if err != nil {
				break
			}
			if event == 'S' {
				return nil
			}
			if event == 'E' {
				return errors.New("Fabo VM stopped with an error")
			}
		}
		var message *C.char
		if C.fabo_vm_stop(machine, &message) != 0 {
			return vmError(message)
		}
		return nil
	}

	boot, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	var hello guestwire.Hello
	stop := context.AfterFunc(boot, func() { _ = reader.SetReadDeadline(time.Now()) })
	event, err := readVMEvent(reader)
	stop()
	if err == nil && event != 'C' {
		err = fmt.Errorf("VM stopped before the guest connected (event %c)", event)
	}
	if err == nil {
		fd := C.fabo_vm_control(machine)
		if fd < 0 {
			err = errors.New("duplicate VM control socket failed")
		} else {
			file := os.NewFile(uintptr(fd), "fabo-vsock")
			conn, err = net.FileConn(file)
			_ = file.Close()
		}
	}
	if err == nil {
		deadline, _ := boot.Deadline()
		err = conn.SetDeadline(deadline)
		if err == nil {
			stopRead := context.AfterFunc(boot, func() { _ = conn.SetDeadline(time.Now()) })
			hello, err = guestwire.ReadHello(conn)
			if err == nil {
				err = guestwire.WriteFrame(conn, guestwire.SetTimeFrame(time.Now()))
			}
			if err == nil {
				// The ping confirms the guest has processed its clock update.
				err = guestwire.CheckPing(conn, 1)
			}
			if stopRead() {
				_ = conn.SetDeadline(time.Time{})
			} else if err == nil {
				err = boot.Err()
			}
		}
	}
	if err != nil {
		return guestConnection{}, fmt.Errorf("start Fabo VM: %w", errors.Join(err, closeVM()))
	}
	return guestConnection{conn: conn, close: closeVM, shares: roots, disk: o.disk != "", relay: dialer.dial, archive: dialer.dial, rosetta: hello.Rosetta}, nil
}

func readVMEvent(reader io.Reader) (byte, error) {
	var data [1]byte
	_, err := io.ReadFull(reader, data[:])
	return data[0], err
}

func (o guestOptions) vmConfiguration() *vmConfiguration {
	var minCPU, maxCPU C.uint32_t
	var minMemory, maxMemory C.uint64_t
	C.fabo_vm_limits(&minCPU, &maxCPU, &minMemory, &maxMemory)
	return &vmConfiguration{
		minCPU: uint(minCPU), maxCPU: uint(maxCPU),
		minMemory: max(1024, (uint64(minMemory)+(1<<20)-1)>>20), maxMemory: uint64(maxMemory) >> 20,
		settings: localengine.VMSettings{CPUs: o.cpus, MemoryMiB: o.memory},
	}
}

func (o guestOptions) withVMSettings(settings localengine.VMSettings) guestOptions {
	o.cpus, o.memory = settings.CPUs, settings.MemoryMiB
	return o
}
