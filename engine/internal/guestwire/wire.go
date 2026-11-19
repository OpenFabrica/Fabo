// Package guestwire implements the framing and startup exchange shared by
// Fabo's host daemon and guest agent. It is Fabo's protocol version 4,
// as defined by upstream's crates/kiri-proto/src/lib.rs.
package guestwire

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"unicode/utf8"
)

const (
	Port       = 1024
	MaxFrame   = 1 << 20
	headerSize = 6
)

// The host-guest protocol version is major.minor, sent at Hello as one
// 16-bit field: the major in the high byte, the minor in the low one. The
// field is unchanged from the plain integers before, which read as 0.x:
// 4 is 0.4. Peers must match exactly.
//
// 0.5 adds SettledMessage, which the host requires before exec into a
// container started early: a peer of another version is refused at Hello
// rather than left waiting for an event it never sends.
//
// 0.6 changes the host-bound relay header (HostBound): it carries the
// protocol and the container's source address, and UDP is relayed to the
// host as well as TCP.
const (
	ProtocolMajor = 0
	ProtocolMinor = 6
	Version       = ProtocolMajor<<8 | ProtocolMinor
)

// ProtocolString spells a Hello version field as major.minor, as "0.6".
func ProtocolString(version uint16) string {
	return fmt.Sprintf("%d.%d", version>>8, version&0xff)
}

type Kind uint16

const (
	HelloMessage   Kind = 1
	Ping           Kind = 2
	Pong           Kind = 3
	Shutdown       Kind = 4
	ErrorMessage   Kind = 5
	StatsRequest   Kind = 6
	StatsMessage   Kind = 7
	MemoryPressure Kind = 30
)

// Frame payloads refer to the buffer supplied to ReadFrame. Reuse it only
// after processing the preceding frame.
type Frame struct {
	Kind    Kind
	Payload []byte
}

func ReadFrame(r io.Reader, scratch []byte) (Frame, error) {
	var header [headerSize]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return Frame{}, err
	}
	n := binary.LittleEndian.Uint32(header[:4])
	if n < 2 || n > MaxFrame-4 {
		return Frame{}, fmt.Errorf("invalid guest frame length %d", n)
	}
	size := int(n) - 2
	if cap(scratch) < size {
		scratch = make([]byte, size)
	} else {
		scratch = scratch[:size]
	}
	if _, err := io.ReadFull(r, scratch); err != nil {
		if errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		return Frame{}, err
	}
	return Frame{Kind(binary.LittleEndian.Uint16(header[4:])), scratch}, nil
}

func WriteFrame(w io.Writer, f Frame) error {
	if len(f.Payload) > MaxFrame-headerSize {
		return fmt.Errorf("guest frame exceeds %d bytes", MaxFrame)
	}
	var header [headerSize]byte
	binary.LittleEndian.PutUint32(header[:4], uint32(len(f.Payload)+2))
	binary.LittleEndian.PutUint16(header[4:], uint16(f.Kind))
	if err := write(w, header[:]); err != nil {
		return err
	}
	if len(f.Payload) == 0 {
		return nil
	}
	return write(w, f.Payload)
}

func write(w io.Writer, b []byte) error {
	n, err := w.Write(b)
	if err == nil && n != len(b) {
		return io.ErrShortWrite
	}
	return err
}

type Hello struct {
	Version                                                  uint16
	InitStartNS, ReadyNS, MemoryTotalKiB, MemoryAvailableKiB uint64
	Rosetta                                                  bool
}

func (h Hello) Frame() Frame {
	b := make([]byte, 2, 34)
	binary.LittleEndian.PutUint16(b, h.Version)
	for _, value := range []uint64{h.InitStartNS, h.ReadyNS, h.MemoryTotalKiB, h.MemoryAvailableKiB} {
		b = binary.LittleEndian.AppendUint64(b, value)
	}
	if h.Rosetta {
		// Go VM extension. The base Hello stays byte-for-byte compatible
		// with the Rust guest and with Go agents that run directly on Linux.
		b = append(b, 1)
	}
	return Frame{HelloMessage, b}
}

func ReadHello(r io.Reader) (Hello, error) {
	f, err := ReadFrame(r, nil)
	if err != nil {
		return Hello{}, err
	}
	if err := expect(f, HelloMessage); err != nil {
		return Hello{}, err
	}
	if len(f.Payload) != 34 && len(f.Payload) != 35 {
		return Hello{}, fmt.Errorf("invalid guest hello size %d", len(f.Payload))
	}
	b := f.Payload
	h := Hello{
		binary.LittleEndian.Uint16(b),
		binary.LittleEndian.Uint64(b[2:]),
		binary.LittleEndian.Uint64(b[10:]),
		binary.LittleEndian.Uint64(b[18:]),
		binary.LittleEndian.Uint64(b[26:]),
		len(b) == 35 && b[34]&1 != 0,
	}
	if h.Version != Version {
		return Hello{}, fmt.Errorf("guest protocol %s, expected %s", ProtocolString(h.Version), ProtocolString(Version))
	}
	return h, nil
}

// CheckPing checks one ping on a startup connection. It requires exclusive
// access to a connection which has completed its Hello exchange.
func CheckPing(rw io.ReadWriter, nonce uint64) error {
	var payload [8]byte
	binary.LittleEndian.PutUint64(payload[:], nonce)
	if err := WriteFrame(rw, Frame{Ping, payload[:]}); err != nil {
		return err
	}
	f, err := readReply(rw, Pong)
	if err != nil {
		return err
	}
	if len(f.Payload) != 8 || binary.LittleEndian.Uint64(f.Payload) != nonce {
		return errors.New("guest pong did not match ping")
	}
	return nil
}

// Stats is the guest's resource sample. Memory sizes are in KiB.
type Stats struct {
	UptimeNS, MemoryTotalKiB, MemoryAvailableKiB, AgentRSSKiB uint64
	MemoryUsedKiB, MemoryStall, OOMKills                      uint64
}

// Frame encodes the current seven-field resource sample.
func (s Stats) Frame() Frame {
	payload := make([]byte, 0, 56)
	for _, value := range [...]uint64{
		s.UptimeNS, s.MemoryTotalKiB, s.MemoryAvailableKiB, s.AgentRSSKiB,
		s.MemoryUsedKiB, s.MemoryStall, s.OOMKills,
	} {
		payload = binary.LittleEndian.AppendUint64(payload, value)
	}
	return Frame{Kind: StatsMessage, Payload: payload}
}

// ReadStats requests a sample on an exclusive startup connection.
func ReadStats(rw io.ReadWriter) (Stats, error) {
	if err := WriteFrame(rw, Frame{Kind: StatsRequest}); err != nil {
		return Stats{}, err
	}
	f, err := readReply(rw, StatsMessage)
	if err != nil {
		return Stats{}, err
	}
	return DecodeStats(f)
}

// Stats asks the guest for a resource sample over the session.
func (s *Session) Stats(ctx context.Context) (Stats, error) {
	frame, err := s.request(ctx, Frame{Kind: StatsRequest}, StatsMessage, "", nil, 0)
	if err != nil {
		return Stats{}, err
	}
	return DecodeStats(frame)
}

// DecodeStats reads a StatsMessage payload.
func DecodeStats(f Frame) (Stats, error) {
	if err := expect(f, StatsMessage); err != nil {
		return Stats{}, err
	}
	// The original message has four words. New agents append three fields.
	if n := len(f.Payload); n < 32 || n > 56 || n%8 != 0 {
		return Stats{}, fmt.Errorf("invalid guest stats size %d", n)
	}
	var words [7]uint64
	for i := 0; i < len(f.Payload)/8; i++ {
		words[i] = binary.LittleEndian.Uint64(f.Payload[i*8:])
	}
	return Stats{words[0], words[1], words[2], words[3], words[4], words[5], words[6]}, nil
}

func readReply(r io.Reader, kind Kind) (Frame, error) {
	for {
		f, err := ReadFrame(r, nil)
		if err != nil {
			return Frame{}, err
		}
		// Pressure notifications can arrive between a request and its reply.
		if f.Kind == MemoryPressure {
			continue
		}
		return f, expect(f, kind)
	}
}

func expect(f Frame, kind Kind) error {
	if f.Kind == kind {
		return nil
	}
	if f.Kind == ErrorMessage && utf8.Valid(f.Payload) {
		return fmt.Errorf("guest: %s", f.Payload)
	}
	return fmt.Errorf("received guest message %d, expected %d", f.Kind, kind)
}
