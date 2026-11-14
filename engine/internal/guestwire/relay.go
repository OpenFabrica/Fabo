package guestwire

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/netip"
)

const (
	// The Windows proxy listener is local to each container namespace.
	WindowsProxyAddress = "127.0.0.2"
	WindowsProxyOption  = "fabo.windows-proxy:"
	WindowsProxyPath    = "/fabo/proxy/relay/"
	HostRelayPort       = 1026
	HostRelayAddress    = "198.19.248.254"
	EngineSocketPath    = "/run/host-services/docker.sock"
	RelayUDPPort        = 1029
	RelayFramedTCPPort  = 1030
	USBPort             = 1040
	MaxRelayDatagram    = 65507
	maxRelayTCPChunk    = 64 << 10
)

// EngineRelayTarget is a reserved host-bound relay header for the Unix API.
// The normal TCP relay only emits IPv4 loopback destinations.
var EngineRelayTarget = netip.AddrPortFrom(netip.IPv4Unspecified(), 1)

type TCPStream interface {
	io.ReadWriteCloser
	CloseWrite() error
}

// BridgeTCP keeps the other direction open when one peer half-closes.
func BridgeTCP(a, b TCPStream) {
	copySide := func(dst, src TCPStream) {
		if _, err := io.Copy(dst, src); err != nil {
			_ = dst.Close()
			_ = src.Close()
		} else {
			_ = dst.CloseWrite()
		}
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		copySide(a, b)
	}()
	copySide(b, a)
	<-done
}

// RelayDial opens a separate VM connection to the requested vsock port.
type RelayDial func(context.Context, uint32) (net.Conn, error)

func WriteRelayTarget(w io.Writer, target netip.AddrPort) error {
	if !target.Addr().Is4() || target.Port() == 0 {
		return fmt.Errorf("invalid relay destination %s", target)
	}
	var header [6]byte
	ip := target.Addr().As4()
	copy(header[:4], ip[:])
	binary.BigEndian.PutUint16(header[4:], target.Port())
	return write(w, header[:])
}

// WriteGuestRelayTarget extends the host-to-guest protocol with IPv6.
// A zero IPv4 destination and port cannot be an existing relay target.
func WriteGuestRelayTarget(w io.Writer, target netip.AddrPort) error {
	if target.Addr().Is4() {
		return WriteRelayTarget(w, target)
	}
	ip := target.Addr()
	if !ip.Is6() || ip.Is4In6() || !ip.IsGlobalUnicast() || ip.Zone() != "" || target.Port() == 0 {
		return fmt.Errorf("invalid guest relay destination %s", target)
	}
	var header [24]byte
	copy(header[6:22], ip.AsSlice())
	binary.BigEndian.PutUint16(header[22:], target.Port())
	return write(w, header[:])
}

func ReadRelayTarget(r io.Reader) (netip.AddrPort, error) {
	return readRelayTarget(r, false)
}

func ReadGuestRelayTarget(r io.Reader) (netip.AddrPort, error) {
	return readRelayTarget(r, true)
}

func readRelayTarget(r io.Reader, guest bool) (netip.AddrPort, error) {
	var header [6]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return netip.AddrPort{}, err
	}
	if guest && header == [6]byte{} {
		var extension [18]byte
		if _, err := io.ReadFull(r, extension[:]); err != nil {
			return netip.AddrPort{}, err
		}
		ip := netip.AddrFrom16([16]byte(extension[:16]))
		port := binary.BigEndian.Uint16(extension[16:])
		if ip.Is4In6() || !ip.IsGlobalUnicast() || port == 0 {
			return netip.AddrPort{}, fmt.Errorf("invalid IPv6 guest relay destination")
		}
		return netip.AddrPortFrom(ip, port), nil
	}
	port := binary.BigEndian.Uint16(header[4:])
	if port == 0 {
		return netip.AddrPort{}, fmt.Errorf("invalid relay destination port")
	}
	return netip.AddrPortFrom(netip.AddrFrom4([4]byte(header[:4])), port), nil
}

func WriteRelayDatagram(w io.Writer, data []byte) error {
	if len(data) > MaxRelayDatagram {
		return fmt.Errorf("relay datagram exceeds %d bytes", MaxRelayDatagram)
	}
	var header [2]byte
	binary.BigEndian.PutUint16(header[:], uint16(len(data)))
	if err := write(w, header[:]); err != nil || len(data) == 0 {
		return err
	}
	return write(w, data)
}

func ReadRelayDatagram(r io.Reader, buffer []byte) (int, error) {
	var header [2]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return 0, err
	}
	n := int(binary.BigEndian.Uint16(header[:]))
	if n > MaxRelayDatagram || n > len(buffer) {
		return 0, fmt.Errorf("invalid relay datagram size %d", n)
	}
	return io.ReadFull(r, buffer[:n])
}

// WriteRelayTCP frames the guest's response and marks EOF with an empty
// frame, independently of any vsock half-close.
func WriteRelayTCP(w io.Writer, source io.Reader) error {
	var header [4]byte
	buffer := make([]byte, maxRelayTCPChunk)
	for {
		n, err := source.Read(buffer)
		if n > 0 {
			binary.BigEndian.PutUint32(header[:], uint32(n))
			if err := write(w, header[:]); err != nil {
				return err
			}
			if err := write(w, buffer[:n]); err != nil {
				return err
			}
		}
		if err == io.EOF {
			return write(w, []byte{0, 0, 0, 0})
		}
		if err != nil {
			return err
		}
	}
}

// ReadRelayTCP copies framed replies through the explicit end marker.
func ReadRelayTCP(w io.Writer, source io.Reader) error {
	var header [4]byte
	buffer := make([]byte, maxRelayTCPChunk)
	for {
		if _, err := io.ReadFull(source, header[:]); err != nil {
			return err
		}
		n := binary.BigEndian.Uint32(header[:])
		if n == 0 {
			return nil
		}
		if n > uint32(len(buffer)) {
			return fmt.Errorf("invalid relay TCP frame size %d", n)
		}
		if _, err := io.ReadFull(source, buffer[:n]); err != nil {
			return err
		}
		if err := write(w, buffer[:n]); err != nil {
			return err
		}
	}
}

// RelayTCPConn adapts a host-to-guest relay to the net.Conn contract used by
// HTTP transports. Writes are raw and reads consume the guest's frames.
type RelayTCPConn struct {
	net.Conn
	left  uint32
	ended bool
}

func NewRelayTCPConn(conn net.Conn) *RelayTCPConn { return &RelayTCPConn{Conn: conn} }

func (c *RelayTCPConn) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if c.ended {
		return 0, io.EOF
	}
	if c.left == 0 {
		var header [4]byte
		if _, err := io.ReadFull(c.Conn, header[:]); err != nil {
			if err == io.EOF {
				err = io.ErrUnexpectedEOF
			}
			return 0, err
		}
		c.left = binary.BigEndian.Uint32(header[:])
		if c.left == 0 {
			c.ended = true
			return 0, io.EOF
		}
		if c.left > maxRelayTCPChunk {
			return 0, fmt.Errorf("invalid relay TCP frame size %d", c.left)
		}
	}
	n, err := c.Conn.Read(p[:min(uint32(len(p)), c.left)])
	c.left -= uint32(n)
	if err == io.EOF {
		err = io.ErrUnexpectedEOF
	}
	return n, err
}
