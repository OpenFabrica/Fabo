package guestwire

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
)

// NetworkJoinPrefix is the Rust Run option for sharing a container's network namespace.
const NetworkJoinPrefix = "netns="

const (
	DeleteBridgeMessage Kind = 78
	NetworkInternal          = "internal=1"
)

// CustomBridge recognizes interface names allocated by the Go host. They
// share Fabo's bridge prefix and stay within Linux's 15-byte name limit.
func CustomBridge(name string) bool {
	suffix, ok := strings.CutPrefix(name, "fabo")
	if !ok || len(suffix) != 10 {
		return false
	}
	for _, ch := range suffix {
		if !(ch >= '0' && ch <= '9' || ch >= 'a' && ch <= 'f') {
			return false
		}
	}
	return true
}

// DeleteBridge waits for teardown before the host releases a subnet. The
// original RemoveBridge command is one-way.
func (s *Session) DeleteBridge(ctx context.Context, name string) error {
	if !CustomBridge(name) {
		return fmt.Errorf("invalid custom bridge")
	}
	var e encoder
	e.text(name)
	if e.err != nil {
		return e.err
	}
	_, err := s.request(ctx, Frame{Kind: DeleteBridgeMessage, Payload: e.payload}, AckMessage, "", nil, 0)
	return err
}

func DecodeDeleteBridge(frame Frame) (string, error) {
	d := runDecoder{data: frame.Payload}
	name := d.text()
	if d.err != nil || len(d.data) != 0 || !CustomBridge(name) {
		return "", fmt.Errorf("invalid custom bridge")
	}
	return name, nil
}

const (
	NetAttachMessage      Kind = 41
	NetDetachMessage      Kind = 42
	MaxEndpoints               = 32
	initialNetworksOption      = "go-networks="
	gatewayPriorityOption      = "gw-priority="
	networkMACOption           = "mac="
)

// ParseEndpointMAC accepts a nonzero unicast Ethernet address for a veth.
func ParseEndpointMAC(value string) (net.HardwareAddr, error) {
	mac, err := net.ParseMAC(value)
	if err != nil || len(mac) != 6 || mac[0]&1 != 0 || string(mac) == "\x00\x00\x00\x00\x00\x00" {
		return nil, fmt.Errorf("invalid MAC address %q", value)
	}
	return mac, nil
}

func (n Network) MAC() (net.HardwareAddr, error) {
	for _, option := range n.Options {
		if value, ok := strings.CutPrefix(option, networkMACOption); ok {
			return ParseEndpointMAC(value)
		}
	}
	return nil, nil
}

func (n *Network) SetMAC(mac net.HardwareAddr) {
	if len(mac) != 0 {
		n.Options = append(n.Options, networkMACOption+mac.String())
	}
}

func (n Network) GatewayPriority() (int16, error) {
	var priority int16
	for _, option := range n.Options {
		if value, ok := strings.CutPrefix(option, gatewayPriorityOption); ok {
			v, err := strconv.ParseInt(value, 10, 16)
			if err != nil {
				return 0, fmt.Errorf("invalid gateway priority")
			}
			priority = int16(v)
		} else if option != NetworkInternal && option != NetworkNoIPv4 &&
			!strings.HasPrefix(option, networkAddress6) && !strings.HasPrefix(option, networkGateway6) &&
			!strings.HasPrefix(option, networkMACOption) {
			return 0, fmt.Errorf("unsupported guest network option")
		}
	}
	return priority, nil
}

func (n *Network) SetGatewayPriority(priority int16) {
	if priority != 0 {
		n.Options = append(n.Options, gatewayPriorityOption+strconv.Itoa(int(priority)))
	}
}

func (s *Session) AttachNetwork(ctx context.Context, id, ifname string, n Network) error {
	var e encoder
	e.text(id)
	e.text(ifname)
	e.text(n.Bridge)
	e.add(n.IP[:]...)
	e.add(n.Prefix)
	e.add(n.Gateway[:]...)
	e.list(n.ExtraHosts)
	e.list(n.DNSNames)
	e.list(n.Options)
	return s.networkAck(ctx, NetAttachMessage, e)
}

func (s *Session) DetachNetwork(ctx context.Context, id, ifname string, n Network) error {
	var e encoder
	e.text(id)
	e.text(ifname)
	e.text(n.Bridge)
	e.add(n.IP[:]...)
	return s.networkAck(ctx, NetDetachMessage, e)
}

func (s *Session) networkAck(ctx context.Context, kind Kind, e encoder) error {
	if e.err != nil {
		return e.err
	}
	_, err := s.request(ctx, Frame{Kind: kind, Payload: e.payload}, AckMessage, "", nil, 0)
	return err
}

func DecodeNetworkChange(frame Frame) (id, ifname string, n Network, err error) {
	d := runDecoder{data: frame.Payload}
	id, ifname, n.Bridge = d.text(), d.text(), d.text()
	copy(n.IP[:], d.take(4))
	if frame.Kind == NetAttachMessage {
		n.Prefix = uint8(d.number(1))
		copy(n.Gateway[:], d.take(4))
		n.ExtraHosts, n.DNSNames = d.list(), d.list()
		if len(d.data) != 0 {
			n.Options = d.list()
		}
	}
	if d.err == nil && len(d.data) != 0 {
		d.err = fmt.Errorf("trailing network change bytes")
	}
	return id, ifname, n, d.err
}
