//go:build cgo

package main

/*
#include "virtualization.h"
*/
import "C"

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"os"
	"runtime/cgo"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"github.com/OpenFabrica/Fabo/engine/internal/guestwire"
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
	bound, engine, err := guestwire.ReadHostBound(guest)
	if err != nil {
		return
	}
	_ = guest.SetReadDeadline(time.Time{})
	dialer := net.Dialer{Timeout: 5 * time.Second}
	var conn net.Conn
	switch {
	case engine && s.engineSocket != "":
		conn, err = dialer.DialContext(s.ctx, "unix", s.engineSocket)
	case engine:
		return
	case bound.UDP:
		s.forwardUDP(guest, bound)
		return
	default:
		target := netip.AddrPortFrom(netip.AddrFrom4([4]byte{127, 0, 0, 1}), bound.Port)
		conn, err = dialer.DialContext(s.ctx, "tcp4", target.String())
	}
	if err != nil {
		return
	}
	defer conn.Close()
	if !engine {
		defer hostPeers.add(false, conn.LocalAddr().String(), bound.Source)()
	}
	backend, ok := conn.(guestwire.TCPStream)
	if !ok {
		return
	}
	closeBackend := context.AfterFunc(s.ctx, func() { _ = backend.Close() })
	defer closeBackend()
	guestwire.BridgeTCP(guest, backend)
}

// forwardUDP relays one container's UDP flow to a port on the loopback:
// datagrams arrive framed from the guest, and each answer goes back framed.
// The flow ends when the guest closes it, after it has been idle.
func (s *hostBound) forwardUDP(guest *net.UnixConn, bound guestwire.HostBound) {
	conn, err := net.DialUDP("udp4", nil, net.UDPAddrFromAddrPort(netip.AddrPortFrom(netip.AddrFrom4([4]byte{127, 0, 0, 1}), bound.Port)))
	if err != nil {
		return
	}
	defer conn.Close()
	defer hostPeers.add(true, conn.LocalAddr().String(), bound.Source)()
	stop := context.AfterFunc(s.ctx, func() { _ = conn.Close() })
	defer stop()
	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, guestwire.MaxRelayDatagram)
		var writeErr error
		for writeErr == nil {
			n, err := conn.Read(buf)
			if err != nil {
				// A server that is not listening answers with ICMP; the
				// flow stays open for when it is.
				if errors.Is(err, syscall.ECONNREFUSED) {
					continue
				}
				_ = guest.Close()
				return
			}
			writeErr = guestwire.WriteRelayDatagram(guest, buf[:n])
		}
	}()
	buf := make([]byte, guestwire.MaxRelayDatagram)
	for {
		n, err := guestwire.ReadRelayDatagram(guest, buf)
		if err != nil {
			break
		}
		_, _ = conn.Write(buf[:n])
	}
	_ = conn.Close()
	<-done
}
