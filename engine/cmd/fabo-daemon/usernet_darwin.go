//go:build cgo

package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/containers/gvisor-tap-vsock/pkg/services/dhcp"
	"github.com/containers/gvisor-tap-vsock/pkg/services/dns"
	"github.com/containers/gvisor-tap-vsock/pkg/services/forwarder"
	"github.com/containers/gvisor-tap-vsock/pkg/tap"
	"github.com/containers/gvisor-tap-vsock/pkg/types"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/network/arp"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/icmp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
)

// The user-mode network's addresses. The subnet only exists between the VM
// and the daemon; it never appears on a macOS interface. The guest keeps
// asking DHCP for eth0, so fabo-init and the bridge code are unchanged.
const (
	userNetSubnet     = "192.168.127.0/24"
	userNetGateway    = "192.168.127.1"
	userNetGuest      = "192.168.127.2"
	userNetGatewayMAC = "5a:94:ef:e4:0c:dd"
	userNetMTU        = 1500
)

// Socket buffer sizes for the frame socket pair, as vfkit and Podman use them:
// the defaults (a few KiB) drop frames under load and cap throughput.
const (
	userNetSendBuffer    = 1 << 20
	userNetReceiveBuffer = 4 << 20
)

// userNetwork is a running user-mode uplink: a gVisor network stack inside
// the daemon that answers the VM's DHCP and DNS and opens its connections
// from macOS, so they follow the Mac's routes, VPNs and resolvers.
//
// It is assembled from gvisor-tap-vsock's parts rather than its
// virtualnetwork package, whose Close leaves the stack and the DHCP and DNS
// servers running: a VM that sleeps and wakes would leak a stack each time.
type userNetwork struct {
	// guestFD is the VM's end of the frame socket pair. The Virtualization
	// framework works on its own duplicate; this one stays open until close,
	// so frames sent while the VM shuts down fill a buffer instead of failing.
	guestFD int
	host    *frameConn
	cancel  context.CancelFunc
	accept  sync.WaitGroup
	servers sync.WaitGroup
	stack   *stack.Stack
	dhcp    *dhcp.Server
	dnsUDP  net.PacketConn
	dnsTCP  net.Listener
}

// startUserNetwork builds the stack for one VM boot with a fixed lease for
// mac, so the VM keeps its address across restarts.
func startUserNetwork(ctx context.Context, mac string) (_ *userNetwork, err error) {
	// n is not the named result: `return nil, err` would clear it before the
	// deferred cleanup runs, leaving the half-built network to leak.
	n := &userNetwork{guestFD: -1}
	defer func() {
		if err != nil {
			n.close()
		}
	}()
	config := &types.Configuration{
		MTU:               userNetMTU,
		Subnet:            userNetSubnet,
		GatewayIP:         userNetGateway,
		GatewayMacAddress: userNetGatewayMAC,
		DHCPStaticLeases:  map[string]string{userNetGuest: mac},
	}
	pool, err := tap.NewIPPool(userNetSubnet)
	if err != nil {
		return nil, err
	}
	if err := errors.Join(pool.Reserve(userNetGateway, userNetGatewayMAC), pool.Reserve(userNetGuest, mac)); err != nil {
		return nil, fmt.Errorf("reserve user-mode network addresses: %w", err)
	}
	link, err := tap.NewLinkEndpoint(false, userNetMTU, userNetGatewayMAC, userNetGateway, nil)
	if err != nil {
		return nil, fmt.Errorf("create user-mode network link: %w", err)
	}
	networkSwitch := tap.NewSwitch(false)
	link.Connect(networkSwitch)
	networkSwitch.Connect(link)
	if n.stack, err = newUserStack(link); err != nil {
		return nil, err
	}

	// Outbound flows: each is opened as an ordinary macOS socket. No address
	// translation entries, so the guest reaches the Mac only where any LAN
	// host could, and the Mac's loopback only through Fabo's vsock relay.
	var natLock sync.Mutex
	nat := map[tcpip.Address]tcpip.Address{}
	n.stack.SetTransportProtocolHandler(tcp.ProtocolNumber, forwarder.TCP(n.stack, nat, &natLock, false, 0, 0).HandlePacket)
	n.stack.SetTransportProtocolHandler(udp.ProtocolNumber, forwarder.UDP(n.stack, nat, &natLock, false).HandlePacket)
	n.stack.SetTransportProtocolHandler(icmp.ProtocolNumber4, forwarder.ICMP(n.stack, nat, &natLock).HandlePacket)

	// DNS on the gateway, answered by the macOS resolver (per-domain
	// resolvers and VPN DNS included).
	gateway := tcpip.FullAddress{NIC: 1, Addr: tcpip.AddrFrom4Slice(net.ParseIP(userNetGateway).To4()), Port: 53}
	if n.dnsUDP, err = gonet.DialUDP(n.stack, &gateway, nil, ipv4.ProtocolNumber); err != nil {
		return nil, fmt.Errorf("listen for user-mode DNS: %w", err)
	}
	if n.dnsTCP, err = gonet.ListenTCP(n.stack, gateway, ipv4.ProtocolNumber); err != nil {
		return nil, fmt.Errorf("listen for user-mode DNS: %w", err)
	}
	resolver, err := dns.New(n.dnsUDP, n.dnsTCP, nil)
	if err != nil {
		return nil, fmt.Errorf("create user-mode DNS: %w", err)
	}
	if n.dhcp, err = dhcp.New(config, n.stack, pool); err != nil {
		return nil, fmt.Errorf("create user-mode DHCP: %w", err)
	}
	// The servers return once close shuts their sockets.
	n.servers.Go(func() { _ = resolver.Serve() })
	n.servers.Go(func() { _ = resolver.ServeTCP() })
	n.servers.Go(func() { _ = n.dhcp.Serve() })

	// The Virtualization framework exchanges one Ethernet frame per datagram.
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_DGRAM, 0)
	if err != nil {
		return nil, fmt.Errorf("create user-mode network socket: %w", err)
	}
	n.guestFD = fds[0]
	for _, fd := range fds {
		syscall.CloseOnExec(fd)
		if err := errors.Join(
			syscall.SetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_SNDBUF, userNetSendBuffer),
			syscall.SetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_RCVBUF, userNetReceiveBuffer),
		); err != nil {
			_ = syscall.Close(fds[1])
			return nil, fmt.Errorf("size user-mode network socket: %w", err)
		}
	}
	file := os.NewFile(uintptr(fds[1]), "fabo-user-network")
	conn, err := net.FileConn(file)
	_ = file.Close()
	if err != nil {
		return nil, fmt.Errorf("open user-mode network socket: %w", err)
	}
	// cancel first: close relies on it whenever host is set.
	ctx, n.cancel = context.WithCancel(ctx)
	n.host = &frameConn{Conn: conn}
	n.accept.Go(func() {
		// Accept returns nil once close has stopped it; an error means the
		// VM lost its network until the next boot.
		if err := networkSwitch.Accept(ctx, n.host, types.VfkitProtocol); err != nil && ctx.Err() == nil {
			log.Printf("user-mode network stopped: %v; restart the VM, or switch the network mode to NAT in Settings", err)
		}
	})
	return n, nil
}

// newUserStack is the IPv4 stack behind the gateway, as gvisor-tap-vsock
// builds it: spoofing and promiscuous mode let it answer for every address
// the guest dials.
func newUserStack(link stack.LinkEndpoint) (*stack.Stack, error) {
	s := stack.New(stack.Options{
		NetworkProtocols:   []stack.NetworkProtocolFactory{ipv4.NewProtocol, arp.NewProtocol},
		TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol, udp.NewProtocol, icmp.NewProtocol4},
	})
	fail := func(what string, err tcpip.Error) (*stack.Stack, error) {
		s.Close()
		s.Wait()
		return nil, fmt.Errorf("user-mode network %s: %s", what, err)
	}
	if err := s.CreateNIC(1, link); err != nil {
		return fail("interface", err)
	}
	address := tcpip.ProtocolAddress{
		Protocol:          ipv4.ProtocolNumber,
		AddressWithPrefix: tcpip.AddrFrom4Slice(net.ParseIP(userNetGateway).To4()).WithPrefix(),
	}
	if err := s.AddProtocolAddress(1, address, stack.AddressProperties{}); err != nil {
		return fail("address", err)
	}
	if err := s.SetSpoofing(1, true); err != nil {
		return fail("spoofing", err)
	}
	if err := s.SetPromiscuousMode(1, true); err != nil {
		return fail("promiscuous mode", err)
	}
	_, subnet, _ := net.ParseCIDR(userNetSubnet)
	route, routeErr := tcpip.NewSubnet(tcpip.AddrFromSlice(subnet.IP.To4()), tcpip.MaskFromBytes(subnet.Mask))
	if routeErr != nil {
		s.Close()
		s.Wait()
		return nil, fmt.Errorf("user-mode network route: %w", routeErr)
	}
	s.SetRouteTable([]tcpip.Route{{Destination: route, NIC: 1}})
	return s, nil
}

// close stops the stack after the VM is gone and waits for every goroutine
// it started. It is safe on a partly built network.
func (n *userNetwork) close() {
	if n.host != nil {
		// Unblock the switch's reads and writes: the guest may no longer
		// read, so a write can wait on a full socket buffer forever.
		n.host.stop()
		n.cancel()
		n.accept.Wait()
		_ = n.host.Conn.Close()
	}
	if n.guestFD >= 0 {
		_ = syscall.Close(n.guestFD)
	}
	if n.dnsUDP != nil {
		_ = n.dnsUDP.Close()
	}
	if n.dnsTCP != nil {
		_ = n.dnsTCP.Close()
	}
	if n.dhcp != nil {
		_ = n.dhcp.Underlying.Close()
	}
	n.servers.Wait()
	if n.stack != nil {
		// Also ends forwarded connections still open to the outside.
		n.stack.Close()
		n.stack.Wait()
	}
}

// ethernetHeaderSize is the shortest frame the switch can parse.
const ethernetHeaderSize = 14

// frameConn is the switch's side of the socket pair. It drops datagrams too
// short to hold an Ethernet header (the switch indexes into the header
// without checking, and a raw socket in a privileged container could send
// one), and lets close end the switch without errors: once stopped, a read
// returns an empty placeholder frame, which the switch drops before it sees
// its context is done, and writes are discarded.
type frameConn struct {
	net.Conn
	stopped atomic.Bool
}

// stop interrupts blocked reads and writes and makes later ones return at once.
func (c *frameConn) stop() {
	c.stopped.Store(true)
	_ = c.Conn.SetDeadline(time.Now())
}

func (c *frameConn) Read(b []byte) (int, error) {
	for {
		n, err := c.Conn.Read(b)
		if c.stopped.Load() {
			return copy(b, make([]byte, ethernetHeaderSize)), nil
		}
		if err != nil || n >= ethernetHeaderSize {
			return n, err
		}
	}
}

func (c *frameConn) Write(b []byte) (int, error) {
	if c.stopped.Load() {
		return len(b), nil
	}
	n, err := c.Conn.Write(b)
	if err != nil && c.stopped.Load() {
		return len(b), nil
	}
	return n, err
}
