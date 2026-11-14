package main

import (
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"net/netip"
	"os"
	"sync"
	"unsafe"

	"github.com/OpenFabrica/Fabo/engine/internal/guestwire"
	"github.com/google/nftables"
	"github.com/google/nftables/expr"
	"golang.org/x/sys/unix"
)

func startHostBound() (func(), error) {
	addr := &net.TCPAddr{IP: net.IPv4zero, Port: guestwire.HostRelayPort}
	listener, err := net.ListenTCP("tcp4", addr)
	if err != nil {
		return nil, err
	}
	stopRedirect, err := redirectLocalHostBound()
	if err != nil {
		_ = listener.Close()
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	slots := make(chan struct{}, 128)
	var workers sync.WaitGroup
	workers.Go(func() {
		for {
			client, err := listener.AcceptTCP()
			if err != nil {
				return
			}
			select {
			case slots <- struct{}{}:
			default:
				_ = client.Close()
				continue
			}
			workers.Go(func() {
				defer func() { <-slots }()
				defer client.Close()
				forwardHost(ctx, client)
			})
		}
	})
	return func() {
		stopRedirect()
		cancel()
		_ = listener.Close()
		workers.Wait()
	}, nil
}

// Host-network workloads share the VM namespace. Their connections take
// the output hook instead of the bridge's prerouting hook.
func redirectLocalHostBound() (func(), error) {
	conn, err := nftables.New()
	if err != nil {
		return nil, err
	}
	table := conn.CreateTable(&nftables.Table{Family: nftables.TableFamilyIPv4, Name: "fabo_go_host_bound"})
	chain := conn.AddChain(&nftables.Chain{Table: table, Name: "output", Type: nftables.ChainTypeNAT,
		Hooknum: nftables.ChainHookOutput, Priority: nftables.ChainPriorityNATDest})
	host := netip.MustParseAddr(guestwire.HostRelayAddress).As4()
	conn.AddRule(&nftables.Rule{Table: table, Chain: chain, Exprs: []expr.Any{
		&expr.Meta{Key: expr.MetaKeyL4PROTO, Register: 1},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte{unix.IPPROTO_TCP}},
		&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseNetworkHeader, Offset: 16, Len: 4},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: host[:]},
		&expr.Immediate{Register: 1, Data: binary.BigEndian.AppendUint16(nil, guestwire.HostRelayPort)},
		&expr.Redir{RegisterProtoMin: 1},
	}})
	if err := conn.Flush(); err != nil {
		return nil, fmt.Errorf("redirect VM host-bound connections: %w", err)
	}
	return func() {
		conn.DelTable(table)
		_ = conn.Flush()
	}, nil
}

func forwardHost(ctx context.Context, client *net.TCPConn) {
	port, err := originalHostPort(client)
	if err != nil {
		return
	}
	host, err := dialHostRelay()
	if err != nil {
		return
	}
	defer host.Close()
	stopClient := context.AfterFunc(ctx, func() { _ = client.Close() })
	defer stopClient()
	stopHost := context.AfterFunc(ctx, func() { _ = host.Close() })
	defer stopHost()
	if err := guestwire.WriteRelayTarget(host, netip.AddrPortFrom(netip.AddrFrom4([4]byte{127, 0, 0, 1}), port)); err != nil {
		return
	}
	guestwire.BridgeTCP(client, host)
}

func originalHostPort(client *net.TCPConn) (uint16, error) {
	raw, err := client.SyscallConn()
	if err != nil {
		return 0, err
	}
	var dst unix.RawSockaddrInet4
	size := uint32(unsafe.Sizeof(dst))
	var callErr unix.Errno
	if err := raw.Control(func(fd uintptr) {
		const soOriginalDst = 80
		_, _, callErr = unix.Syscall6(unix.SYS_GETSOCKOPT, fd, unix.SOL_IP, soOriginalDst,
			uintptr(unsafe.Pointer(&dst)), uintptr(unsafe.Pointer(&size)), 0)
	}); err != nil {
		return 0, err
	}
	if callErr != 0 {
		return 0, callErr
	}
	// Only transparently redirected requests can reach the host relay.
	if dst.Family != unix.AF_INET || netip.AddrFrom4(dst.Addr).String() != guestwire.HostRelayAddress {
		return 0, fmt.Errorf("request was not redirected from the host address")
	}
	port := binary.BigEndian.Uint16((*[2]byte)(unsafe.Pointer(&dst.Port))[:])
	if port == 0 {
		return 0, fmt.Errorf("invalid original port")
	}
	return port, nil
}

type hostRelayFile struct{ *os.File }

func (f hostRelayFile) CloseWrite() error {
	raw, err := f.SyscallConn()
	if err != nil {
		return err
	}
	var callErr error
	if err := raw.Control(func(fd uintptr) { callErr = unix.Shutdown(int(fd), unix.SHUT_WR) }); err != nil {
		return err
	}
	return callErr
}

func dialHostRelay() (hostRelayFile, error) {
	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM|unix.SOCK_NONBLOCK|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return hostRelayFile{}, err
	}
	if err := unix.Connect(fd, &unix.SockaddrVM{CID: unix.VMADDR_CID_HOST, Port: guestwire.HostRelayPort}); err != nil {
		if err == unix.EINPROGRESS {
			n, pollErr := unix.Poll([]unix.PollFd{{Fd: int32(fd), Events: unix.POLLOUT}}, 2000)
			switch {
			case pollErr != nil:
				err = pollErr
			case n == 0:
				err = context.DeadlineExceeded
			default:
				status, optionErr := unix.GetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_ERROR)
				err = optionErr
				if err == nil && status != 0 {
					err = unix.Errno(status)
				}
			}
		}
		if err != nil {
			_ = unix.Close(fd)
			return hostRelayFile{}, err
		}
	}
	return hostRelayFile{os.NewFile(uintptr(fd), "fabo-host-bound")}, nil
}
