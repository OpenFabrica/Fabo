package main

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/OpenFabrica/FabriBay/engine/internal/guestwire"
	"golang.org/x/sys/unix"
)

const vhci = "/sys/devices/platform/vhci_hcd.0"

func startUSB() (func(), error) {
	if _, err := os.Stat(vhci + "/status"); err != nil {
		return nil, err
	}
	listener, err := listenRelay(guestwire.USBPort)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	var workers sync.WaitGroup
	var ports sync.Mutex
	workers.Go(func() {
		serveVSock(ctx, listener, make(chan struct{}, 32), &workers, func(_ context.Context, conn *os.File) {
			if err := attachUSB(conn, &ports); err != nil {
				log.Printf("attach USB: %v", err)
			}
		})
	})
	return func() {
		cancel()
		_ = listener.Close()
		workers.Wait()
	}, nil
}

func attachUSB(conn *os.File, ports *sync.Mutex) error {
	if err := conn.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
		return err
	}
	var request [8]byte
	if _, err := io.ReadFull(conn, request[:]); err != nil {
		return err
	}
	speed, device := binary.LittleEndian.Uint32(request[:4]), binary.LittleEndian.Uint32(request[4:])
	reply := func(value int32) error {
		var data [4]byte
		binary.LittleEndian.PutUint32(data[:], uint32(value))
		_, err := conn.Write(data[:])
		return err
	}
	reject := func(err error) error {
		var errno unix.Errno
		if !errors.As(err, &errno) {
			errno = unix.EIO
		}
		return errors.Join(err, reply(-int32(errno)))
	}
	if speed < 1 || speed > 6 {
		return reject(unix.EINVAL)
	}
	// Reserving a port and handing its socket to vhci are one operation.
	// The next request must read status after the kernel owns this port.
	ports.Lock()
	defer ports.Unlock()
	status, err := os.ReadFile(vhci + "/status")
	if err != nil {
		return reject(err)
	}
	hub := "hs"
	if speed >= 5 {
		hub = "ss"
	}
	port, ok := freeUSBPort(string(status), hub)
	if !ok {
		return reject(unix.EBUSY)
	}
	kernel, err := os.OpenFile(vhci+"/attach", os.O_WRONLY, 0)
	if err != nil {
		return reject(err)
	}
	defer kernel.Close()
	raw, err := conn.SyscallConn()
	if err != nil {
		return reject(err)
	}
	// Send the port first, as with the Rust agent. The kernel starts
	// USB/IP traffic as soon as it gets a reference to the stream.
	if err := reply(port); err != nil {
		return err
	}
	if err := conn.SetDeadline(time.Time{}); err != nil {
		return err
	}
	var attachErr error
	err = raw.Control(func(fd uintptr) {
		if attachErr = unix.SetNonblock(int(fd), false); attachErr == nil {
			_, attachErr = fmt.Fprintf(kernel, "%d %d %d %d", port, fd, device, speed)
		}
	})
	// Closing the Go file only drops this reference. vhci keeps the
	// socket until the host disconnects.
	return errors.Join(err, attachErr)
}

func freeUSBPort(status, hub string) (int32, bool) {
	for line := range strings.SplitSeq(status, "\n") {
		f := strings.Fields(line)
		if len(f) < 3 || f[0] != hub {
			continue
		}
		// VDEV_ST_NULL is the kernel's free-port state.
		state, err := strconv.ParseUint(f[2], 10, 32)
		if err != nil || state != 4 {
			continue
		}
		if port, err := strconv.ParseInt(f[1], 10, 32); err == nil && port >= 0 {
			return int32(port), true
		}
	}
	return 0, false
}
