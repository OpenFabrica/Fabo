// Package wslbridge carries Docker API connections between Windows and the
// engine in WSL2 over one long-lived `wsl.exe -- fabo bridge` process,
// multiplexed on its stdin and stdout. TCP through WSL's localhost relay
// loses half-closes, which `docker run` and `attach` depend on; the frames
// here keep them.
//
// Both ends live in this package so the Windows host (Mux) and the WSL side
// (Serve) always agree, and so the whole path can be tested on any OS. The
// frame layout and the flow control are byte-compatible with upstream
// Kiri's crates/kiri/src/bridge.rs and crates/kiri-win/src/serve.rs.
//
// Flow control: each side may have at most Window bytes of a connection in
// flight towards the other, and the other acknowledges (KindAck) what it
// has handed on. A sender out of credit waits on that connection alone, so
// a container that stops reading its stdin, or a client that stops reading
// its output, holds up only its own connection. The bridge says it does
// this with an ACK for connection 0 when it starts; the Windows side asks
// for it per connection by sending 1 with OPEN.
package wslbridge

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sync"
)

// Frame kinds: kind (1 byte), connection id (u32 BE), length (u32 BE), data.
const (
	KindOpen  byte = 1
	KindData  byte = 2
	KindEOF   byte = 3 // no more data from this side (a half-close)
	KindClose byte = 4 // the connection is gone, both ways
	KindAck   byte = 5 // credit back: u32 BE bytes handed on; for id 0, the bridge's hello
)

const (
	headerSize = 9
	// MaxFrame is the largest frame either side accepts. Both send at most
	// chunkSize bytes of data per frame; the bound stops a corrupt length
	// from allocating up to 4 GiB.
	MaxFrame = 1 << 20
	// Window is how many bytes of one connection one way may be
	// unacknowledged: the most a connection holds in memory on each side.
	Window = 4 << 20
	// AckEvery is how many handed-on bytes are acknowledged at once.
	AckEvery = Window / 4
	// limit closes a connection whose peer sends past its credit (or,
	// without credit, one whose receiver has stopped reading).
	limit = 2 * Window
	// limitNoCredit is the Windows side's bound for an older bridge that
	// does no flow control: what used to be 256 frames.
	limitNoCredit = 256 * 64 * 1024
	chunkSize     = 64 << 10
)

// ErrFrameTooLarge reports a frame length over MaxFrame.
var ErrFrameTooLarge = errors.New("wslbridge: frame too large")

// Frame is one decoded frame.
type Frame struct {
	Kind byte
	ID   uint32
	Data []byte
}

// header encodes a frame header.
func header(kind byte, id uint32, n int) [headerSize]byte {
	var h [headerSize]byte
	h[0] = kind
	binary.BigEndian.PutUint32(h[1:5], id)
	binary.BigEndian.PutUint32(h[5:9], uint32(n))
	return h
}

// ReadFrame reads the next frame. It returns io.EOF only when the stream
// ends cleanly between frames.
func ReadFrame(r io.Reader) (Frame, error) {
	var h [headerSize]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return Frame{}, err
	}
	n := binary.BigEndian.Uint32(h[5:9])
	// The length comes off the wire: refuse it before allocating it.
	if n > MaxFrame {
		return Frame{}, fmt.Errorf("%w: %d bytes", ErrFrameTooLarge, n)
	}
	f := Frame{Kind: h[0], ID: binary.BigEndian.Uint32(h[1:5]), Data: make([]byte, n)}
	if _, err := io.ReadFull(r, f.Data); err != nil {
		if errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		return Frame{}, err
	}
	return f, nil
}

// WriteFrame writes one frame to w (unbuffered callers get two writes).
func WriteFrame(w io.Writer, kind byte, id uint32, data []byte) error {
	h := header(kind, id, len(data))
	if _, err := w.Write(h[:]); err != nil {
		return err
	}
	if len(data) == 0 {
		return nil
	}
	_, err := w.Write(data)
	return err
}

// frameWriter serialises frames from many goroutines onto one stream, each
// frame whole and flushed at once.
type frameWriter struct {
	mu sync.Mutex
	w  *bufio.Writer
}

// newFrameWriter buffers w so a header and its data leave in one write.
func newFrameWriter(w io.Writer) *frameWriter {
	return &frameWriter{w: bufio.NewWriterSize(w, chunkSize+headerSize)}
}

// send writes and flushes one frame.
func (f *frameWriter) send(kind byte, id uint32, data []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := WriteFrame(f.w, kind, id, data); err != nil {
		return err
	}
	return f.w.Flush()
}

// ackData encodes an ACK's byte count.
func ackData(n int) []byte {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], uint32(n))
	return b[:]
}

// ackCount decodes an ACK's byte count.
func ackCount(data []byte) (int, bool) {
	if len(data) < 4 {
		return 0, false
	}
	return int(binary.BigEndian.Uint32(data[:4])), true
}
