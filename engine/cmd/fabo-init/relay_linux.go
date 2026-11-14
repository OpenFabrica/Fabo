package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"sync"
	"time"

	"github.com/OpenFabrica/Fabo/engine/internal/guestwire"
	"golang.org/x/sys/unix"
)

func startPortRelays() (func(), error) {
	tcp, err := listenRelay(guestwire.RelayFramedTCPPort)
	if err != nil {
		return nil, err
	}
	udp, err := listenRelay(guestwire.RelayUDPPort)
	if err != nil {
		_ = tcp.Close()
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	slots := make(chan struct{}, 1024)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		serveVSock(ctx, tcp, slots, &wg, withRelayTarget(forwardTCP))
	}()
	go func() {
		defer wg.Done()
		serveVSock(ctx, udp, slots, &wg, withRelayTarget(forwardUDP))
	}()
	return func() {
		cancel()
		_ = tcp.Close()
		_ = udp.Close()
		wg.Wait()
	}, nil
}

func listenRelay(port uint32) (*os.File, error) {
	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM|unix.SOCK_NONBLOCK|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	if err := unix.Bind(fd, &unix.SockaddrVM{CID: unix.VMADDR_CID_ANY, Port: port}); err != nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("bind vsock port %d: %w", port, err)
	}
	if err := unix.Listen(fd, 128); err != nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("listen on vsock port %d: %w", port, err)
	}
	return os.NewFile(uintptr(fd), "vsock-listener"), nil
}

func serveVSock(ctx context.Context, listener *os.File, slots chan struct{}, wg *sync.WaitGroup, handle func(context.Context, *os.File)) {
	raw, err := listener.SyscallConn()
	if err != nil {
		return
	}
	for {
		var fd int
		var addr unix.Sockaddr
		var acceptErr error
		if err := raw.Read(func(socket uintptr) bool {
			fd, addr, acceptErr = unix.Accept4(int(socket), unix.SOCK_NONBLOCK|unix.SOCK_CLOEXEC)
			return acceptErr != unix.EAGAIN
		}); err != nil {
			return
		}
		if acceptErr != nil {
			if acceptErr == unix.EINTR || acceptErr == unix.ECONNABORTED {
				continue
			}
			return
		}
		peer, ok := addr.(*unix.SockaddrVM)
		if !ok || peer.CID != unix.VMADDR_CID_HOST {
			_ = unix.Close(fd)
			continue
		}
		select {
		case slots <- struct{}{}:
		default:
			_ = unix.Close(fd)
			continue
		}

		host := os.NewFile(uintptr(fd), "vsock-relay")
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-slots }()
			defer host.Close()
			stop := context.AfterFunc(ctx, func() { _ = host.Close() })
			defer stop()
			handle(ctx, host)
		}()
	}
}

func withRelayTarget(relay func(context.Context, *os.File, netip.AddrPort)) func(context.Context, *os.File) {
	return func(ctx context.Context, host *os.File) {
		if err := host.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
			return
		}
		target, err := guestwire.ReadGuestRelayTarget(host)
		if err != nil {
			return
		}
		_ = host.SetReadDeadline(time.Time{})
		relay(ctx, host, target)
	}
}

func forwardTCP(ctx context.Context, host *os.File, target netip.AddrPort) {
	dialer := net.Dialer{Timeout: 5 * time.Second}
	conn, err := dialer.DialContext(ctx, "tcp", target.String())
	if err != nil {
		return
	}
	backend := conn.(*net.TCPConn)
	defer backend.Close()
	stop := context.AfterFunc(ctx, func() { _ = backend.Close() })
	defer stop()
	up := make(chan struct{})
	go func() {
		defer close(up)
		if _, err := io.Copy(backend, host); err != nil {
			_ = backend.Close()
		} else {
			_ = backend.CloseWrite()
		}
	}()
	if err := guestwire.WriteRelayTCP(host, backend); err != nil {
		_ = host.Close()
		_ = backend.Close()
	}
	<-up
}

func forwardUDP(ctx context.Context, host *os.File, target netip.AddrPort) {
	backend, err := net.DialUDP("udp", nil, net.UDPAddrFromAddrPort(target))
	if err != nil {
		return
	}
	defer backend.Close()
	stop := context.AfterFunc(ctx, func() { _ = backend.Close() })
	defer stop()
	up := make(chan struct{})
	go func() {
		defer close(up)
		defer backend.Close()
		buf := make([]byte, guestwire.MaxRelayDatagram)
		for {
			n, err := guestwire.ReadRelayDatagram(host, buf)
			if err != nil {
				return
			}
			if _, err := backend.Write(buf[:n]); err != nil && !errors.Is(err, unix.ECONNREFUSED) {
				return
			}
		}
	}()
	buf := make([]byte, guestwire.MaxRelayDatagram)
	for {
		n, err := backend.Read(buf)
		if errors.Is(err, unix.ECONNREFUSED) {
			continue
		}
		if err != nil || guestwire.WriteRelayDatagram(host, buf[:n]) != nil {
			break
		}
	}
	_ = host.Close()
	<-up
}
