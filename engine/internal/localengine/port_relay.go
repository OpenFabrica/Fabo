//go:build linux || darwin

package localengine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"sync"
	"syscall"
	"time"

	"github.com/OpenFabrica/FabriBay/engine/internal/guestwire"
)

type publishedPorts struct {
	bound   []portBinding
	relays  []func()
	sockets []io.Closer
	ctx     context.Context
	cancel  context.CancelFunc
	slots   chan struct{}
	vmRelay guestwire.RelayDial

	mu     sync.Mutex
	active map[io.Closer]struct{}
}

func (e *Engine) reservePorts(bindings []portBinding, target netip.Addr) (*publishedPorts, error) {
	if e.domains == nil {
		return reservePorts(bindings, target, e.guestRelay)
	}
	return reservePortsWithYield(bindings, target, e.guestRelay, e.domains.Yield)
}

// reservePorts binds every requested endpoint before the guest starts.
// A failed reservation closes the endpoints already bound for this run.
func reservePorts(bindings []portBinding, target netip.Addr, relay guestwire.RelayDial) (*publishedPorts, error) {
	return reservePortsWithYield(bindings, target, relay, nil)
}

func reservePortsWithYield(bindings []portBinding, target netip.Addr, relay guestwire.RelayDial, yield func(uint16) bool) (*publishedPorts, error) {
	if len(bindings) != 0 && !target.IsValid() {
		return nil, fmt.Errorf("%w: published ports require a container address", errInvalid)
	}
	ctx, cancel := context.WithCancel(context.Background())
	p := &publishedPorts{ctx: ctx, cancel: cancel, slots: make(chan struct{}, 1024), active: make(map[io.Closer]struct{}), vmRelay: relay}
	bind := func(b portBinding) error {
		err := p.bind(b, target)
		if b.Protocol == "tcp" && errors.Is(err, errConflict) && yield != nil && yield(b.HostPort) {
			return p.bind(b, target)
		}
		return err
	}
	for _, b := range bindings {
		if b.HostIP != "" {
			if err := bind(b); err != nil {
				p.close()
				return nil, err
			}
			continue
		}
		b.HostIP = "0.0.0.0"
		if err := bind(b); err != nil {
			p.close()
			return nil, err
		}
		b.HostIP, b.HostPort = "::", p.bound[len(p.bound)-1].HostPort
		// As in Fabo, the IPv6 wildcard is optional when unavailable.
		_ = bind(b)
	}
	return p, nil
}

func (p *publishedPorts) bind(b portBinding, target netip.Addr) error {
	ip, err := netip.ParseAddr(b.HostIP)
	if err != nil {
		return err
	}
	addr := netip.AddrPortFrom(ip, b.HostPort)
	dst := netip.AddrPortFrom(target, b.ContainerPort)
	family := "4"
	if ip.Is6() {
		family = "6"
	}
	switch b.Protocol {
	case "tcp":
		socket, err := net.ListenTCP("tcp"+family, net.TCPAddrFromAddrPort(addr))
		if err != nil {
			return portBindError(addr, err)
		}
		b.HostPort = socket.Addr().(*net.TCPAddr).AddrPort().Port()
		if p.relayLoops(ip, b.HostPort, dst) {
			_ = socket.Close()
			return fmt.Errorf("%w: published port would relay to itself in host networking", errInvalid)
		}
		p.sockets = append(p.sockets, socket)
		p.relays = append(p.relays, func() { p.serveTCP(socket, dst) })
	case "udp":
		socket, err := net.ListenUDP("udp"+family, net.UDPAddrFromAddrPort(addr))
		if err != nil {
			return portBindError(addr, err)
		}
		b.HostPort = socket.LocalAddr().(*net.UDPAddr).AddrPort().Port()
		if p.relayLoops(ip, b.HostPort, dst) {
			_ = socket.Close()
			return fmt.Errorf("%w: published port would relay to itself in host networking", errInvalid)
		}
		p.sockets = append(p.sockets, socket)
		p.relays = append(p.relays, func() { p.serveUDP(socket, dst) })
	default:
		return fmt.Errorf("%w: invalid port protocol %q", errInvalid, b.Protocol)
	}
	p.bound = append(p.bound, b)
	return nil
}

func (p *publishedPorts) relayLoops(ip netip.Addr, port uint16, target netip.AddrPort) bool {
	return p.vmRelay == nil && target.Addr().IsLoopback() && port == target.Port() &&
		(ip == target.Addr() || ip.Is4() && ip.IsUnspecified())
}

func portBindError(addr netip.AddrPort, err error) error {
	if errors.Is(err, syscall.EADDRINUSE) {
		return fmt.Errorf("%w: bind %s: port is already allocated", errConflict, addr)
	}
	return fmt.Errorf("bind %s: %w", addr, err)
}

func (p *publishedPorts) start() {
	for _, relay := range p.relays {
		go relay()
	}
}

func (p *publishedPorts) close() {
	p.cancel()
	for _, socket := range p.sockets {
		_ = socket.Close()
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for conn := range p.active {
		_ = conn.Close()
	}
}

func (p *publishedPorts) track(conn io.Closer) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ctx.Err() != nil {
		_ = conn.Close()
		return false
	}
	p.active[conn] = struct{}{}
	return true
}

func (p *publishedPorts) release(conn io.Closer) {
	_ = conn.Close()
	p.mu.Lock()
	delete(p.active, conn)
	p.mu.Unlock()
}

func (p *publishedPorts) takeSlot() bool {
	select {
	case p.slots <- struct{}{}:
		return true
	default:
		return false
	}
}

func (p *publishedPorts) serveTCP(socket *net.TCPListener, dst netip.AddrPort) {
	for {
		client, err := socket.AcceptTCP()
		if err != nil {
			return
		}
		if !p.takeSlot() {
			_ = client.Close()
			continue
		}
		go func() {
			defer func() { <-p.slots }()
			if !p.track(client) {
				return
			}
			defer p.release(client)
			upstream, err := p.dial("tcp", guestwire.RelayFramedTCPPort, dst)
			if err != nil || !p.track(upstream) {
				return
			}
			defer p.release(upstream)

			done := make(chan struct{})
			go func() {
				defer close(done)
				if _, err := io.Copy(upstream, client); err != nil {
					_ = upstream.Close()
				} else if socket, ok := upstream.(interface{ CloseWrite() error }); ok {
					_ = socket.CloseWrite()
				}
			}()
			if p.vmRelay == nil {
				_, err = io.Copy(client, upstream)
			} else {
				err = guestwire.ReadRelayTCP(client, upstream)
			}
			if err != nil {
				_ = client.Close()
				_ = upstream.Close()
			} else {
				_ = client.CloseWrite()
			}
			<-done
		}()
	}
}

func (p *publishedPorts) serveUDP(socket *net.UDPConn, dst netip.AddrPort) {
	const idle = 60 * time.Second
	sessions := make(map[netip.AddrPort]net.Conn)
	var mu sync.Mutex
	buf := make([]byte, 65535)
	for {
		n, peer, err := socket.ReadFromUDPAddrPort(buf)
		if err != nil {
			return
		}
		mu.Lock()
		upstream := sessions[peer]
		if upstream == nil && p.takeSlot() {
			upstream, err = p.dial("udp", guestwire.RelayUDPPort, dst)
			if err == nil && p.track(upstream) {
				sessions[peer] = upstream
				go func() {
					defer func() {
						mu.Lock()
						delete(sessions, peer)
						mu.Unlock()
						p.release(upstream)
						<-p.slots
					}()
					reply := make([]byte, 65535)
					for {
						_ = upstream.SetReadDeadline(time.Now().Add(idle))
						var n int
						var err error
						if p.vmRelay == nil {
							n, err = upstream.Read(reply)
						} else {
							n, err = guestwire.ReadRelayDatagram(upstream, reply)
						}
						if err != nil {
							return
						}
						if _, err := socket.WriteToUDPAddrPort(reply[:n], peer); err != nil {
							return
						}
					}
				}()
			} else {
				<-p.slots
				upstream = nil
			}
		}
		mu.Unlock()
		if upstream != nil {
			_ = upstream.SetReadDeadline(time.Now().Add(idle))
			_ = upstream.SetWriteDeadline(time.Now().Add(time.Second))
			if p.vmRelay == nil {
				_, _ = upstream.Write(buf[:n])
			} else if n <= guestwire.MaxRelayDatagram {
				if err := guestwire.WriteRelayDatagram(upstream, buf[:n]); err != nil {
					_ = upstream.Close()
				}
			}
		}
	}
}

func (p *publishedPorts) dial(network string, relayPort uint32, dst netip.AddrPort) (net.Conn, error) {
	return dialPublishedPort(p.ctx, p.vmRelay, network, relayPort, dst)
}

func dialPublishedPort(ctx context.Context, relay guestwire.RelayDial, network string, relayPort uint32, dst netip.AddrPort) (net.Conn, error) {
	if relay == nil {
		dialer := net.Dialer{Timeout: 5 * time.Second}
		return dialer.DialContext(ctx, network, dst.String())
	}
	conn, err := relay(ctx, relayPort)
	if err != nil {
		return nil, err
	}
	err = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if err == nil {
		err = guestwire.WriteGuestRelayTarget(conn, dst)
	}
	if err == nil {
		err = conn.SetWriteDeadline(time.Time{})
	}
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	return conn, nil
}
