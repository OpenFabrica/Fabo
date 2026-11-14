//go:build cgo

package main

/*
#include "virtualization.h"
*/
import "C"

import (
	"context"
	"fmt"
	"net"
	"os"
	"sync"
	"unsafe"
)

type vmRelayDialer struct {
	mu      sync.RWMutex
	machine unsafe.Pointer
}

func (d *vmRelayDialer) close() {
	d.mu.Lock()
	d.machine = nil
	d.mu.Unlock()
}

func (d *vmRelayDialer) dial(ctx context.Context, port uint32) (net.Conn, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if d.machine == nil {
		return nil, net.ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var fd C.int
	var message *C.char
	relay := C.fabo_vm_connect(d.machine, C.uint32_t(port), &fd, &message)
	if relay == nil {
		return nil, vmError(message)
	}
	file := os.NewFile(uintptr(fd), "fabo-vsock-relay")
	conn, err := net.FileConn(file)
	_ = file.Close()
	if err != nil {
		C.fabo_vm_close_relay(relay)
		return nil, err
	}
	socket, ok := conn.(*net.UnixConn)
	if !ok {
		_ = conn.Close()
		C.fabo_vm_close_relay(relay)
		return nil, fmt.Errorf("VM relay is not a Unix stream")
	}
	stream := &vmRelayConn{UnixConn: socket, relay: relay}
	if err := ctx.Err(); err != nil {
		_ = stream.Close()
		return nil, err
	}
	return stream, nil
}

type vmRelayConn struct {
	*net.UnixConn
	relay unsafe.Pointer
	once  sync.Once
}

func (c *vmRelayConn) Close() error {
	var err error
	c.once.Do(func() {
		err = c.UnixConn.Close()
		C.fabo_vm_close_relay(c.relay)
	})
	return err
}
