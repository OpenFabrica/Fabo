package main

// UDP from containers to host.docker.internal. The nat rules that redirect
// TCP to the host relay (guestagent's forwardBridge, and the output chain
// in hostbound_linux.go) redirect UDP to port 1026 here too. A redirected
// datagram no longer says which port it was sent to, so the original port
// is read from its conntrack entry. Each container address is a flow: one
// vsock connection to the host, which sends the datagrams to that port on
// the Mac's loopback and relays the answers back. Answers leave from this
// socket and conntrack rewrites their source to the host address and port
// the container sent to.
//
// TPROXY would keep the original destination without conntrack, but the
// VM's bridges call iptables (br_netfilter), which runs prerouting before
// the IP layer drops the socket TPROXY assigned.

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sync"
	"time"

	"github.com/OpenFabrica/Fabo/engine/internal/guestwire"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// hostUDPIdle ends a flow that carried nothing for this long.
const hostUDPIdle = 60 * time.Second

// hostUDPFlow is one container address talking to one host port.
type hostUDPFlow struct {
	source netip.AddrPort
	host   hostRelayFile
	idle   *time.Timer
	// writeMu orders the frames written to the host.
	writeMu sync.Mutex
}

// startHostBoundUDP starts the UDP relay on the redirect port.
func startHostBoundUDP() (func(), error) {
	listener, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: guestwire.HostRelayPort})
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	var mu sync.Mutex
	flows := map[netip.AddrPort]*hostUDPFlow{}
	slots := make(chan struct{}, 128)
	var workers sync.WaitGroup
	workers.Go(func() {
		buf := make([]byte, guestwire.MaxRelayDatagram)
		for {
			n, from, err := listener.ReadFromUDPAddrPort(buf)
			if err != nil {
				return
			}
			source := netip.AddrPortFrom(from.Addr().Unmap(), from.Port())
			mu.Lock()
			flow := flows[source]
			mu.Unlock()
			if flow == nil {
				// The slot comes first: with every slot taken, a datagram
				// is dropped without reading the whole conntrack table.
				select {
				case slots <- struct{}{}:
				default:
					continue
				}
				port, ok := redirectedUDPPort(source)
				if !ok {
					<-slots
					continue
				}
				flow, err = openHostUDPFlow(source, port)
				if err != nil {
					<-slots
					continue
				}
				mu.Lock()
				flows[source] = flow
				mu.Unlock()
				workers.Go(func() {
					defer func() {
						mu.Lock()
						delete(flows, source)
						mu.Unlock()
						<-slots
					}()
					flow.run(ctx, listener)
				})
			}
			flow.send(buf[:n])
		}
	})
	return func() {
		cancel()
		_ = listener.Close()
		mu.Lock()
		for _, flow := range flows {
			flow.close()
		}
		mu.Unlock()
		workers.Wait()
	}, nil
}

// redirectedUDPPort finds the port a container sent a redirected datagram
// to, from the conntrack entry of its flow.
func redirectedUDPPort(source netip.AddrPort) (uint16, bool) {
	flows, err := netlink.ConntrackTableList(netlink.ConntrackTable, unix.AF_INET)
	if err != nil {
		return 0, false
	}
	return originalUDPPort(flows, source)
}

// originalUDPPort picks, among conntrack flows, the UDP flow from source
// to the host address redirected to the relay, and returns the port it was
// sent to.
func originalUDPPort(flows []*netlink.ConntrackFlow, source netip.AddrPort) (uint16, bool) {
	host := netip.MustParseAddr(guestwire.HostRelayAddress)
	for _, f := range flows {
		if f.Forward.Protocol != unix.IPPROTO_UDP || f.Forward.SrcPort != source.Port() || f.Reverse.SrcPort != guestwire.HostRelayPort {
			continue
		}
		src, ok1 := netip.AddrFromSlice(f.Forward.SrcIP)
		dst, ok2 := netip.AddrFromSlice(f.Forward.DstIP)
		if ok1 && ok2 && src.Unmap() == source.Addr() && dst.Unmap() == host && f.Forward.DstPort != 0 {
			return f.Forward.DstPort, true
		}
	}
	return 0, false
}

// openHostUDPFlow connects a flow to the host.
func openHostUDPFlow(source netip.AddrPort, port uint16) (*hostUDPFlow, error) {
	if !source.Addr().Is4() {
		return nil, fmt.Errorf("host-bound UDP from %s: IPv4 only", source)
	}
	host, err := dialHostRelay()
	if err != nil {
		return nil, err
	}
	if err := guestwire.WriteHostBound(host, guestwire.HostBound{UDP: true, Source: source, Port: port}); err != nil {
		_ = host.Close()
		return nil, err
	}
	f := &hostUDPFlow{source: source, host: host}
	// The clock starts before the first datagram is sent, from the
	// listener's goroutine, so it exists before either side touches it.
	f.idle = time.AfterFunc(hostUDPIdle, f.close)
	return f, nil
}

// send relays one datagram from the container to the host.
func (f *hostUDPFlow) send(data []byte) {
	f.touch()
	f.writeMu.Lock()
	defer f.writeMu.Unlock()
	_ = f.host.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if err := guestwire.WriteRelayDatagram(f.host, data); err != nil {
		f.close()
	}
}

// touch restarts the flow's idle clock.
func (f *hostUDPFlow) touch() { f.idle.Reset(hostUDPIdle) }

// close ends the flow; run returns.
func (f *hostUDPFlow) close() { _ = f.host.Close() }

// run relays answers from the host back to the container through the
// listener, until the flow is idle or the relay stops.
func (f *hostUDPFlow) run(ctx context.Context, listener *net.UDPConn) {
	defer f.idle.Stop()
	stop := context.AfterFunc(ctx, f.close)
	defer stop()
	defer f.close()
	to := net.UDPAddrFromAddrPort(f.source)
	buf := make([]byte, guestwire.MaxRelayDatagram)
	for {
		n, err := guestwire.ReadRelayDatagram(f.host, buf)
		if err != nil {
			return
		}
		f.touch()
		if _, err := listener.WriteToUDP(buf[:n], to); err != nil && errors.Is(err, net.ErrClosed) {
			return
		}
	}
}
