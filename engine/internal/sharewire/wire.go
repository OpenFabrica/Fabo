// Package sharewire is the wire protocol between the Fabo share client
// (hosted by the daemon) and a fabo-edge relay.
//
// The frame layout follows upstream Kiri's kiri-share-proto byte for byte:
// every frame is `len: u32 LE | kind: u16 LE | payload`, where len counts
// kind plus payload; strings are `u32 LE length | UTF-8 bytes`; trailing
// optional fields read as zero or empty when a peer leaves them out.
//
// Session: the client dials the edge over TLS with ALPN [ALPN] and sends
// [Hello]; the edge answers [Welcome] or [Refused]. Each visitor connection
// becomes a numbered stream: the edge sends [Open], both sides exchange
// [Data], [Window] (credit), [Close] (half-close) and [Reset]. Either side
// may [Ping]; the edge ends a session with [Goodbye].
//
// Compatibility scope: Fabo clients never set the attest or lanes feature
// bits, so a conforming edge never sends kinds 13-17 (Challenge, Attest,
// Join, Joined, Chunk). Those kind numbers stay reserved and decode as
// [ErrUnsupported], which ends the session like any other protocol error,
// just as an upstream client does.
package sharewire

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"unicode/utf8"
)

const (
	// QuotaExhaustedPrefix distinguishes recoverable account quota refusals.
	QuotaExhaustedPrefix = "quota exhausted:"
	// Protocol is the session protocol version sent in Hello.
	Protocol uint16 = 1
	// FeatureWindow tells the edge this client uses Welcome.Window for
	// what it sends, so the edge may grant more than InitialWindow.
	FeatureWindow uint32 = 4
	// ALPN is what tunnel clients offer, so the edge can tell them from
	// visitors and API callers on the same port.
	ALPN = "fabo-share/1"
	// Header is the length prefix plus kind.
	Header = 6
	// MaxChunk is the largest Data payload a sender puts in one frame.
	MaxChunk = 32 * 1024
	// MaxFrame is the largest frame either side accepts, header included.
	MaxFrame = 64 * 1024
	// InitialWindow is the per-stream credit before any Window frame, and
	// the fixed window for edge-to-client data.
	InitialWindow uint32 = 256 * 1024
)

// Kind identifies a frame.
type Kind uint16

const (
	KindHello   Kind = 1
	KindWelcome Kind = 2
	KindRefused Kind = 3
	KindOpen    Kind = 4
	KindData    Kind = 5
	KindWindow  Kind = 6
	KindClose   Kind = 7
	KindReset   Kind = 8
	KindPing    Kind = 9
	KindPong    Kind = 10
	KindNotice  Kind = 11
	KindGoodbye Kind = 12
	// Kinds 13-17 (Challenge, Attest, Join, Joined, Chunk) are reserved
	// for upstream's attestation and lanes, which Fabo does not use.
	kindReservedFirst Kind = 13
	kindReservedLast  Kind = 17
)

var (
	// ErrTruncated reports a frame shorter than its fields.
	ErrTruncated = errors.New("sharewire: truncated frame")
	// ErrBadUTF8 reports a string field that is not UTF-8.
	ErrBadUTF8 = errors.New("sharewire: string is not UTF-8")
	// ErrUnsupported reports a reserved kind Fabo does not implement.
	ErrUnsupported = errors.New("sharewire: unsupported frame kind")
)

// Msg is one decoded or to-be-encoded frame.
type Msg interface {
	// Kind returns the frame kind.
	Kind() Kind
	appendPayload(b []byte) []byte
}

// Hello is the client's first frame.
type Hello struct {
	Protocol uint16
	// Token is the account token from `fabo share login`.
	Token string
	// Device is the random id of this install.
	Device string
	// Name is the preferred first label of the hostname.
	Name string
	// Target is what the tunnel points at, for the owner's dashboard.
	Target string
	// Client is the client version, such as `fabo 0.3.0`.
	Client string
	// Features are Feature* bits; read as 0 when left out.
	Features uint32
	// Machine is a stable hash of the computer's hardware id.
	Machine string
	// Host is the computer's name as people know it.
	Host string
}

// Welcome tells the client its tunnel is live.
type Welcome struct {
	URL string
	// Rate is bytes per second per direction; 0 means no limit.
	Rate uint32
	// Remaining is bytes left this month; math.MaxUint64 means no cap.
	Remaining uint64
	// Warning means browsers see a warning page first.
	Warning bool
	// Domains are verified custom domains as comma-separated URLs.
	Domains string
	// Lanes and LaneKey are upstream's extra connections; Fabo edges send 0.
	Lanes   uint32
	LaneKey string
	// Window is the client's per-stream send credit; 0 means InitialWindow.
	Window uint32
}

// SendWindow returns the bytes the client may have in flight per stream.
func (w Welcome) SendWindow() uint32 {
	if w.Window == 0 {
		return InitialWindow
	}
	return w.Window
}

// Refused ends a session before it starts; the edge closes afterwards.
type Refused struct{ Reason string }

// Open announces a visitor connection as a new stream.
type Open struct {
	Stream uint32
	Remote string
}

// Data carries stream bytes. A decoded Data aliases the frame buffer.
type Data struct {
	Stream uint32
	Data   []byte
}

// Window grants Credit more bytes on a stream.
type Window struct{ Stream, Credit uint32 }

// Close half-closes a stream: the sender will send no more data.
type Close struct{ Stream uint32 }

// Reset drops a stream in both directions.
type Reset struct{ Stream uint32 }

// Ping asks the peer to answer Pong with the same value.
type Ping struct{ Value uint64 }

// Pong answers Ping.
type Pong struct{ Value uint64 }

// Notice is text the person running the tunnel should see.
type Notice struct{ Text string }

// Goodbye ends a session; with Reconnect the client should dial again.
type Goodbye struct {
	Reconnect bool
	Reason    string
}

func (Hello) Kind() Kind   { return KindHello }
func (Welcome) Kind() Kind { return KindWelcome }
func (Refused) Kind() Kind { return KindRefused }
func (Open) Kind() Kind    { return KindOpen }
func (Data) Kind() Kind    { return KindData }
func (Window) Kind() Kind  { return KindWindow }
func (Close) Kind() Kind   { return KindClose }
func (Reset) Kind() Kind   { return KindReset }
func (Ping) Kind() Kind    { return KindPing }
func (Pong) Kind() Kind    { return KindPong }
func (Notice) Kind() Kind  { return KindNotice }
func (Goodbye) Kind() Kind { return KindGoodbye }

func putU32(b []byte, v uint32) []byte { return binary.LittleEndian.AppendUint32(b, v) }

func putStr(b []byte, s string) []byte {
	b = putU32(b, uint32(len(s)))
	return append(b, s...)
}

func putBool(b []byte, v bool) []byte {
	if v {
		return append(b, 1)
	}
	return append(b, 0)
}

func (h Hello) appendPayload(b []byte) []byte {
	b = binary.LittleEndian.AppendUint16(b, h.Protocol)
	for _, s := range []string{h.Token, h.Device, h.Name, h.Target, h.Client} {
		b = putStr(b, s)
	}
	b = putU32(b, h.Features)
	b = putStr(b, h.Machine)
	return putStr(b, h.Host)
}

func (w Welcome) appendPayload(b []byte) []byte {
	b = putStr(b, w.URL)
	b = putU32(b, w.Rate)
	b = binary.LittleEndian.AppendUint64(b, w.Remaining)
	b = putBool(b, w.Warning)
	b = putStr(b, w.Domains)
	b = putU32(b, w.Lanes)
	b = putStr(b, w.LaneKey)
	return putU32(b, w.Window)
}

func (m Refused) appendPayload(b []byte) []byte { return append(b, m.Reason...) }
func (m Open) appendPayload(b []byte) []byte    { return append(putU32(b, m.Stream), m.Remote...) }
func (m Data) appendPayload(b []byte) []byte    { return append(putU32(b, m.Stream), m.Data...) }
func (m Window) appendPayload(b []byte) []byte  { return putU32(putU32(b, m.Stream), m.Credit) }
func (m Close) appendPayload(b []byte) []byte   { return putU32(b, m.Stream) }
func (m Reset) appendPayload(b []byte) []byte   { return putU32(b, m.Stream) }
func (m Ping) appendPayload(b []byte) []byte    { return binary.LittleEndian.AppendUint64(b, m.Value) }
func (m Pong) appendPayload(b []byte) []byte    { return binary.LittleEndian.AppendUint64(b, m.Value) }
func (m Notice) appendPayload(b []byte) []byte  { return append(b, m.Text...) }
func (m Goodbye) appendPayload(b []byte) []byte { return append(putBool(b, m.Reconnect), m.Reason...) }

// Append encodes m as one frame appended to b.
func Append(b []byte, m Msg) []byte {
	start := len(b)
	b = append(b, 0, 0, 0, 0, 0, 0)
	b = m.appendPayload(b)
	binary.LittleEndian.PutUint32(b[start:], uint32(len(b)-start-4))
	binary.LittleEndian.PutUint16(b[start+4:], uint16(m.Kind()))
	return b
}

// FrameLen returns the total size of the frame whose 4-byte length prefix
// is header, rejecting frames shorter than a kind or larger than MaxFrame.
func FrameLen(header [4]byte) (int, error) {
	n := int(binary.LittleEndian.Uint32(header[:]))
	if n < 2 || n+4 > MaxFrame {
		return 0, fmt.Errorf("sharewire: frame of %d bytes is too large", n)
	}
	return n + 4, nil
}

// ReadFrame reads one whole frame into buf (grown when needed) and returns
// it. The result is only valid until the next call with the same buf.
func ReadFrame(r io.Reader, buf []byte) ([]byte, error) {
	var header [4]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return nil, err
	}
	n, err := FrameLen(header)
	if err != nil {
		return nil, err
	}
	if cap(buf) < n {
		buf = make([]byte, n)
	}
	buf = buf[:n]
	copy(buf, header[:])
	if _, err := io.ReadFull(r, buf[4:]); err != nil {
		if errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		return nil, err
	}
	return buf, nil
}

// reader walks a frame payload; trailing optional fields read as zero.
type reader struct {
	buf []byte
	err error
}

func (r *reader) take(n int) []byte {
	if r.err != nil {
		return nil
	}
	if n < 0 || n > len(r.buf) {
		r.err = ErrTruncated
		return nil
	}
	s := r.buf[:n]
	r.buf = r.buf[n:]
	return s
}

func (r *reader) u8() uint8 {
	if s := r.take(1); s != nil {
		return s[0]
	}
	return 0
}

func (r *reader) u16() uint16 {
	if s := r.take(2); s != nil {
		return binary.LittleEndian.Uint16(s)
	}
	return 0
}

func (r *reader) u32() uint32 {
	if s := r.take(4); s != nil {
		return binary.LittleEndian.Uint32(s)
	}
	return 0
}

func (r *reader) u64() uint64 {
	if s := r.take(8); s != nil {
		return binary.LittleEndian.Uint64(s)
	}
	return 0
}

func (r *reader) text(b []byte) string {
	if r.err == nil && !utf8.Valid(b) {
		r.err = ErrBadUTF8
	}
	return string(b)
}

func (r *reader) str() string {
	n := r.u32()
	if r.err != nil {
		return ""
	}
	if uint64(n) > uint64(len(r.buf)) {
		r.err = ErrTruncated
		return ""
	}
	return r.text(r.take(int(n)))
}

// optStr reads a trailing string older peers leave out: empty when absent.
func (r *reader) optStr() string {
	if r.err == nil && len(r.buf) == 0 {
		return ""
	}
	return r.str()
}

// optU32 reads a trailing u32 older peers leave out: 0 when absent.
func (r *reader) optU32() uint32 {
	if r.err == nil && len(r.buf) == 0 {
		return 0
	}
	return r.u32()
}

func (r *reader) rest() []byte {
	s := r.buf
	r.buf = nil
	return s
}

// Decode decodes one complete frame (header included). Data payloads
// alias frame; copy them before the buffer is reused.
func Decode(frame []byte) (Msg, error) {
	if len(frame) < Header {
		return nil, ErrTruncated
	}
	kind := Kind(binary.LittleEndian.Uint16(frame[4:]))
	r := &reader{buf: frame[Header:]}
	var m Msg
	switch kind {
	case KindHello:
		m = Hello{Protocol: r.u16(), Token: r.str(), Device: r.str(), Name: r.str(), Target: r.str(), Client: r.str(),
			Features: r.optU32(), Machine: r.optStr(), Host: r.optStr()}
	case KindWelcome:
		m = Welcome{URL: r.str(), Rate: r.u32(), Remaining: r.u64(), Warning: r.u8() != 0,
			Domains: r.optStr(), Lanes: r.optU32(), LaneKey: r.optStr(), Window: r.optU32()}
	case KindRefused:
		m = Refused{Reason: r.text(r.rest())}
	case KindOpen:
		m = Open{Stream: r.u32(), Remote: r.text(r.rest())}
	case KindData:
		m = Data{Stream: r.u32(), Data: r.rest()}
	case KindWindow:
		m = Window{Stream: r.u32(), Credit: r.u32()}
	case KindClose:
		m = Close{Stream: r.u32()}
	case KindReset:
		m = Reset{Stream: r.u32()}
	case KindPing:
		m = Ping{Value: r.u64()}
	case KindPong:
		m = Pong{Value: r.u64()}
	case KindNotice:
		m = Notice{Text: r.text(r.rest())}
	case KindGoodbye:
		m = Goodbye{Reconnect: r.u8() != 0, Reason: r.text(r.rest())}
	default:
		if kind >= kindReservedFirst && kind <= kindReservedLast {
			return nil, fmt.Errorf("%w %d", ErrUnsupported, kind)
		}
		return nil, fmt.Errorf("sharewire: unknown frame kind %d", kind)
	}
	if r.err != nil {
		return nil, r.err
	}
	return m, nil
}
