//go:build cgo

package main

/*
#include "virtualization.h"
*/
import "C"

import (
	"context"
	"net"
	"net/netip"
	"os"
	"runtime/cgo"
	"sync"
	"time"
	"unsafe"

	"github.com/OpenFabrica/FabriBay/engine/internal/guestwire"
)

type hostBound struct {
	ctx          context.Context
	slots        chan struct{}
	workers      sync.WaitGroup
	engineSocket string
}

//export faboAcceptHost
func faboAcceptHost(handle C.uintptr_t, fd C.int, relay unsafe.Pointer) C.int {
	server := cgo.Handle(handle).Value().(*hostBound)
	if server.ctx.Err() != nil {
		return 0
	}
	select {
	case server.slots <- struct{}{}:
	default:
		return 0
	}
	server.workers.Go(func() {
		defer func() { <-server.slots }()
		defer C.fabo_vm_close_relay(relay)
		file := os.NewFile(uintptr(fd), "fabo-host-bound")
		conn, err := net.FileConn(file)
		_ = file.Close()
		if err != nil {
			return
		}
		defer conn.Close()
		guest, ok := conn.(*net.UnixConn)
		if !ok {
			return
		}
		server.forward(guest)
	})
	return 1
}

func (s *hostBound) forward(guest *net.UnixConn) {
	stop := context.AfterFunc(s.ctx, func() { _ = guest.Close() })
	defer stop()
	if err := guest.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return
	}
	target, err := guestwire.ReadRelayTarget(guest)
	if err != nil {
		return
	}
	_ = guest.SetReadDeadline(time.Time{})
	dialer := net.Dialer{Timeout: 5 * time.Second}
	var conn net.Conn
	if target == guestwire.EngineRelayTarget && s.engineSocket != "" {
		conn, err = dialer.DialContext(s.ctx, "unix", s.engineSocket)
	} else if target.Addr() == netip.AddrFrom4([4]byte{127, 0, 0, 1}) {
		conn, err = dialer.DialContext(s.ctx, "tcp4", target.String())
	} else {
		return
	}
	if err != nil {
		return
	}
	defer conn.Close()
	backend, ok := conn.(guestwire.TCPStream)
	if !ok {
		return
	}
	closeBackend := context.AfterFunc(s.ctx, func() { _ = backend.Close() })
	defer closeBackend()
	guestwire.BridgeTCP(guest, backend)
}
