package guestwire

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
)

const (
	LayerQueryMessage  Kind = 8
	LayerStatusMessage Kind = 9
	LayerBeginMessage  Kind = 10
	LayerStoredMessage Kind = 11
)

// HasLayer reports whether the guest has already unpacked the blob.
func (s *Session) HasLayer(ctx context.Context, digest string) (bool, error) {
	var e encoder
	e.text(digest)
	if e.err != nil {
		return false, e.err
	}
	reply, err := s.request(ctx, Frame{Kind: LayerQueryMessage, Payload: e.payload}, LayerStatusMessage, "", nil, 0)
	if err != nil {
		return false, err
	}
	if len(reply.Payload) != 1 {
		return false, fmt.Errorf("invalid LayerStatus payload size %d", len(reply.Payload))
	}
	return reply.Payload[0] != 0, nil
}

type LayerStored struct {
	UnpackedBytes uint64
	ElapsedNS     uint64
}

// SendLayer sends exactly size bytes from blob after the LayerBegin frame
// and waits for the guest to finish unpacking. As with Start, ctx controls
// admission; close the session to interrupt a transfer on the connection.
func (s *Session) SendLayer(ctx context.Context, digest, mediaType string, blob io.Reader, size int64) (LayerStored, error) {
	if size < 0 || blob == nil {
		return LayerStored{}, fmt.Errorf("invalid layer reader or size")
	}
	var e encoder
	e.text(digest)
	e.text(mediaType)
	e.u64(uint64(size))
	if e.err != nil {
		return LayerStored{}, e.err
	}
	reply, err := s.request(ctx, Frame{Kind: LayerBeginMessage, Payload: e.payload}, LayerStoredMessage, "", blob, size)
	if err != nil {
		return LayerStored{}, err
	}
	if len(reply.Payload) != 16 {
		return LayerStored{}, fmt.Errorf("invalid LayerStored payload size %d", len(reply.Payload))
	}
	return LayerStored{binary.LittleEndian.Uint64(reply.Payload), binary.LittleEndian.Uint64(reply.Payload[8:])}, nil
}

// DecodeLayerQuery returns the digest carried by a query.
func DecodeLayerQuery(f Frame) (string, error) {
	digest, rest, err := takeBytes(f.Payload)
	if err != nil {
		return "", err
	}
	if len(rest) != 0 {
		return "", fmt.Errorf("invalid layer query")
	}
	return string(digest), nil
}

type LayerBegin struct {
	Digest, MediaType string
	Size              uint64
}

func DecodeLayerBegin(f Frame) (LayerBegin, error) {
	digest, rest, err := takeBytes(f.Payload)
	if err != nil {
		return LayerBegin{}, err
	}
	media, size, err := takeBytes(rest)
	if err != nil {
		return LayerBegin{}, err
	}
	if len(size) != 8 {
		return LayerBegin{}, fmt.Errorf("invalid layer size field")
	}
	return LayerBegin{string(digest), string(media), binary.LittleEndian.Uint64(size)}, nil
}

func (l LayerStored) Frame() Frame {
	body := binary.LittleEndian.AppendUint64(nil, l.UnpackedBytes)
	body = binary.LittleEndian.AppendUint64(body, l.ElapsedNS)
	return Frame{Kind: LayerStoredMessage, Payload: body}
}

// LayerExpansionLimit bounds extraction and validation of the same layer on
// the host and in the guest.
func LayerExpansionLimit(size uint64) int64 {
	return int64(min(max(min(size, (64<<30)/256)*256, 1<<30), 64<<30))
}
