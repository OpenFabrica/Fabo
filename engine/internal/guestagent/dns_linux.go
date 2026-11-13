package guestagent

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/libracoder/kiri/engine/internal/guestwire"
	"golang.org/x/net/dns/dnsmessage"
)

const dnsTimeout = 3 * time.Second

type dnsUpstream struct {
	servers []netip.Addr
	dial    func(context.Context, string, string) (net.Conn, error)
}

type bridgeDNS struct {
	ctx       context.Context
	cancel    context.CancelFunc
	udp       *net.UDPConn
	tcp       *net.TCPListener
	subnet    netip.Prefix
	internal  bool
	hostRelay bool
	lookup    func(string) ([]netip.Addr, bool)
	upstream  dnsUpstream
	slots     chan struct{}
	wait      sync.WaitGroup

	mu    sync.RWMutex
	names map[string]map[netip.Addr]struct{}
}

func listenBridgeDNS(gateway netip.Prefix, internal, hostRelay bool) (*bridgeDNS, error) {
	ip := gateway.Addr().AsSlice()
	family := "4"
	if gateway.Addr().Is6() {
		family = "6"
	}
	udp, err := net.ListenUDP("udp"+family, &net.UDPAddr{IP: ip, Port: 53})
	if err != nil {
		return nil, err
	}
	tcp, err := net.ListenTCP("tcp"+family, &net.TCPAddr{IP: ip, Port: 53})
	if err != nil {
		_ = udp.Close()
		return nil, err
	}
	return newBridgeDNS(udp, tcp, gateway.Masked(), internal, hostRelay, nil, dnsUpstream{}), nil
}

func newBridgeDNS(udp *net.UDPConn, tcp *net.TCPListener, subnet netip.Prefix, internal, hostRelay bool, lookup func(string) ([]netip.Addr, bool), upstream dnsUpstream) *bridgeDNS {
	ctx, cancel := context.WithCancel(context.Background())
	d := &bridgeDNS{
		ctx: ctx, cancel: cancel, udp: udp, tcp: tcp, subnet: subnet, internal: internal, hostRelay: hostRelay, lookup: lookup, upstream: upstream,
		slots: make(chan struct{}, 64), names: make(map[string]map[netip.Addr]struct{}),
	}
	d.wait.Go(d.serveUDP)
	d.wait.Go(d.serveTCP)
	return d
}

func (d *bridgeDNS) addresses(name string) []netip.Addr {
	d.mu.RLock()
	defer d.mu.RUnlock()
	var result []netip.Addr
	for ip := range d.names[name] {
		result = append(result, ip)
	}
	return result
}

func (d *bridgeDNS) close() {
	d.cancel()
	_ = d.udp.Close()
	_ = d.tcp.Close()
	d.wait.Wait()
}

func dnsKey(name string) string {
	return strings.ToLower(strings.TrimSuffix(name, "."))
}

var hostRelayNames = []string{"host.docker.internal", "host.kiri.internal", "gateway.docker.internal"}

func isHostRelayName(name string) bool {
	return slices.Contains(hostRelayNames, name)
}

func (d *bridgeDNS) register(names []string, ip netip.Addr) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, alias := range names {
		key := dnsKey(alias)
		if d.names[key] == nil {
			d.names[key] = make(map[netip.Addr]struct{})
		}
		d.names[key][ip] = struct{}{}
	}
}

func (d *bridgeDNS) unregister(names []string, ip netip.Addr) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, alias := range names {
		key := dnsKey(alias)
		delete(d.names[key], ip)
		if len(d.names[key]) == 0 {
			delete(d.names, key)
		}
	}
}

func (d *bridgeDNS) takeSlot() bool {
	select {
	case d.slots <- struct{}{}:
		return true
	default:
		return false
	}
}

func (d *bridgeDNS) serveUDP() {
	buf := make([]byte, 65535)
	for {
		n, addr, err := d.udp.ReadFromUDPAddrPort(buf)
		if err != nil {
			return
		}
		if !d.subnet.Contains(addr.Addr().Unmap()) || !d.takeSlot() {
			continue
		}
		query := bytes.Clone(buf[:n])
		d.wait.Go(func() {
			defer func() { <-d.slots }()
			if answer := d.answer(query, false); len(answer) != 0 {
				_, _ = d.udp.WriteToUDPAddrPort(answer, addr)
			}
		})
	}
}

func (d *bridgeDNS) serveTCP() {
	for {
		conn, err := d.tcp.AcceptTCP()
		if err != nil {
			return
		}
		remote := conn.RemoteAddr().(*net.TCPAddr).AddrPort().Addr().Unmap()
		if !d.subnet.Contains(remote) || !d.takeSlot() {
			_ = conn.Close()
			continue
		}
		d.wait.Go(func() {
			defer func() { <-d.slots }()
			defer conn.Close()
			stop := context.AfterFunc(d.ctx, func() { _ = conn.Close() })
			defer stop()
			for {
				_ = conn.SetDeadline(time.Now().Add(dnsTimeout))
				query, err := readDNSMessage(conn)
				if err != nil {
					return
				}
				answer := d.answer(query, true)
				if len(answer) == 0 || writeDNSMessage(conn, answer) != nil {
					return
				}
			}
		})
	}
}

func (d *bridgeDNS) answer(query []byte, tcp bool) []byte {
	var req dnsmessage.Message
	if err := req.Unpack(query); err != nil || req.Response || len(req.Questions) != 1 || req.OpCode != 0 {
		return nil
	}
	q := req.Questions[0]
	key := dnsKey(q.Name.String())
	addresses, internal := d.addresses(key), d.internal
	if d.lookup != nil {
		addresses, internal = d.lookup(key)
	}
	if d.hostRelay && !internal && isHostRelayName(key) {
		addresses = []netip.Addr{netip.MustParseAddr(guestwire.HostRelayAddress)}
	}
	canForward := !internal || len(d.upstream.servers) != 0
	resp := dnsmessage.Message{
		Header: dnsmessage.Header{
			ID: req.ID, Response: true, RecursionDesired: req.RecursionDesired,
			RecursionAvailable: canForward,
		},
		Questions: req.Questions,
	}
	if len(addresses) == 0 {
		if canForward {
			if forwarded := d.forward(query, tcp); len(forwarded) != 0 {
				return forwarded
			}
			resp.RCode = dnsmessage.RCodeServerFailure
		} else {
			resp.RCode = dnsmessage.RCodeNameError
		}
	} else {
		resp.Authoritative = true
		if q.Class == dnsmessage.ClassINET {
			slices.SortFunc(addresses, netip.Addr.Compare)
			for _, ip := range slices.Compact(addresses) {
				var body dnsmessage.ResourceBody
				switch {
				case ip.Is4() && (q.Type == dnsmessage.TypeA || q.Type == dnsmessage.TypeALL):
					body = &dnsmessage.AResource{A: ip.As4()}
				case ip.Is6() && (q.Type == dnsmessage.TypeAAAA || q.Type == dnsmessage.TypeALL):
					body = &dnsmessage.AAAAResource{AAAA: ip.As16()}
				default:
					continue
				}
				resp.Answers = append(resp.Answers, dnsmessage.Resource{
					Header: dnsmessage.ResourceHeader{Name: q.Name, Class: dnsmessage.ClassINET, TTL: 0},
					Body:   body,
				})
			}
		}
	}
	wire, err := resp.Pack()
	if err != nil || len(wire) > 65535 || !tcp && len(wire) > 512 {
		resp.Truncated = true
		resp.Answers = nil
		wire, _ = resp.Pack()
	}
	return wire
}

func (d *bridgeDNS) forward(query []byte, tcp bool) []byte {
	ctx, cancel := context.WithTimeout(d.ctx, dnsTimeout)
	defer cancel()
	deadline, _ := ctx.Deadline()
	upstream := d.upstream
	custom := len(upstream.servers) != 0
	if !custom {
		upstream = dnsUpstream{servers: upstreamResolvers(), dial: (&net.Dialer{}).DialContext}
	}
	var failed []byte
	for i, addr := range upstream.servers {
		addr = addr.Unmap()
		if custom && addr == loopbackResolver || !custom && d.subnet.Contains(addr) {
			continue
		}
		attempt, done := context.WithTimeout(ctx, time.Until(deadline)/time.Duration(len(upstream.servers)-i))
		response, err := exchangeDNS(attempt, upstream, netip.AddrPortFrom(addr, 53), query, tcp)
		done()
		if err != nil || len(response) < 12 || !bytes.Equal(response[:2], query[:2]) || response[2]&0x80 == 0 {
			continue
		}
		code := dnsmessage.RCode(response[3] & 0xf)
		if custom && (code == dnsmessage.RCodeServerFailure || code == dnsmessage.RCodeRefused) {
			failed = response
			continue
		}
		return response
	}
	return failed
}

func exchangeDNS(ctx context.Context, upstream dnsUpstream, addr netip.AddrPort, query []byte, tcp bool) ([]byte, error) {
	network := "udp"
	if tcp {
		network = "tcp"
	}
	if addr.Addr().Is4() {
		network += "4"
	} else {
		network += "6"
	}
	conn, err := upstream.dial(ctx, network, addr.String())
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	deadline, _ := ctx.Deadline()
	_ = conn.SetDeadline(deadline)
	if tcp {
		if err := writeDNSMessage(conn, query); err != nil {
			return nil, err
		}
		return readDNSMessage(conn)
	}
	if _, err := conn.Write(query); err != nil {
		return nil, err
	}
	response := make([]byte, 65535)
	n, err := conn.Read(response)
	return response[:n], err
}

func readDNSMessage(r io.Reader) ([]byte, error) {
	var size [2]byte
	if _, err := io.ReadFull(r, size[:]); err != nil {
		return nil, err
	}
	buf := make([]byte, binary.BigEndian.Uint16(size[:]))
	_, err := io.ReadFull(r, buf)
	return buf, err
}

func writeDNSMessage(w io.Writer, data []byte) error {
	frame := binary.BigEndian.AppendUint16(make([]byte, 0, len(data)+2), uint16(len(data)))
	_, err := w.Write(append(frame, data...))
	return err
}
