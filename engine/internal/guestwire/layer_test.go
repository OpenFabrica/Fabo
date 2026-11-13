package guestwire

import (
	"bytes"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

func TestLayerTransferKeepsFollowingRequestAligned(t *testing.T) {
	host, guest := net.Pipe()
	_ = host.SetDeadline(time.Now().Add(3 * time.Second))
	_ = guest.SetDeadline(time.Now().Add(3 * time.Second))
	defer guest.Close()
	s := NewSession(host, nil)
	defer s.Close()
	peer := make(chan error, 1)
	go func() {
		peer <- layerPeer(guest)
	}()
	if have, err := s.HasLayer(t.Context(), "digest"); err != nil || have {
		t.Fatalf("first lookup: have=%v err=%v", have, err)
	}
	data := bytes.NewBufferString("abcNEXT")
	stored, err := s.SendLayer(t.Context(), "digest", "application/test", data, 3)
	if err != nil || stored != (LayerStored{UnpackedBytes: 9, ElapsedNS: 12}) || data.String() != "NEXT" {
		t.Fatalf("upload: %+v %v, remaining=%q", stored, err, data.String())
	}
	if have, err := s.HasLayer(t.Context(), "digest"); err != nil || !have {
		t.Fatalf("second lookup: have=%v err=%v", have, err)
	}
	if err := <-peer; err != nil {
		t.Fatal(err)
	}
}

func layerPeer(conn net.Conn) error {
	for _, have := range []byte{0, 1} {
		f, err := ReadFrame(conn, nil)
		if err != nil {
			return err
		}
		digest, rest, err := takeBytes(f.Payload)
		if err != nil || f.Kind != LayerQueryMessage || string(digest) != "digest" || len(rest) != 0 {
			return errors.New("bad LayerQuery")
		}
		if err := WriteFrame(conn, Frame{Kind: LayerStatusMessage, Payload: []byte{have}}); err != nil {
			return err
		}
		if have == 1 {
			return nil
		}
		f, err = ReadFrame(conn, nil)
		if err != nil {
			return err
		}
		var begin encoder
		begin.text("digest")
		begin.text("application/test")
		begin.u64(3)
		if f.Kind != LayerBeginMessage || !bytes.Equal(f.Payload, begin.payload) {
			return errors.New("bad LayerBegin")
		}
		var data [3]byte
		if _, err := io.ReadFull(conn, data[:]); err != nil {
			return err
		}
		if string(data[:]) != "abc" {
			return errors.New("bad layer bytes")
		}
		var stored encoder
		stored.u64(9)
		stored.u64(12)
		if err := WriteFrame(conn, Frame{Kind: LayerStoredMessage, Payload: stored.payload}); err != nil {
			return err
		}
	}
	return nil
}

func TestLayerShortReadInvalidatesSession(t *testing.T) {
	host, guest := net.Pipe()
	_ = host.SetDeadline(time.Now().Add(3 * time.Second))
	_ = guest.SetDeadline(time.Now().Add(3 * time.Second))
	defer guest.Close()
	s := NewSession(host, nil)
	defer s.Close()
	go func() {
		_, _ = ReadFrame(guest, nil)
		_, _ = io.Copy(io.Discard, guest)
	}()
	if _, err := s.SendLayer(t.Context(), "digest", "application/test", strings.NewReader("abc"), 4); !errors.Is(err, io.EOF) {
		t.Fatalf("expected EOF, got %v", err)
	}
	if _, err := s.HasLayer(t.Context(), "digest"); !errors.Is(err, io.EOF) {
		t.Fatalf("session accepted request after short blob: %v", err)
	}
}

func TestLayerEarlyRejectionInterruptsWriter(t *testing.T) {
	host, guest := net.Pipe()
	_ = host.SetDeadline(time.Now().Add(3 * time.Second))
	_ = guest.SetDeadline(time.Now().Add(3 * time.Second))
	defer guest.Close()
	s := NewSession(host, nil)
	defer s.Close()
	go func() {
		if _, err := ReadFrame(guest, nil); err == nil {
			_ = WriteFrame(guest, Frame{Kind: ErrorMessage, Payload: []byte("cannot create layer directory")})
		}
	}()
	if _, err := s.SendLayer(t.Context(), "digest", "application/test", strings.NewReader("abc"), 3); err == nil || !strings.Contains(err.Error(), "cannot create layer directory") {
		t.Fatalf("expected guest error, got %v", err)
	}
	select {
	case <-s.Done():
	default:
		t.Fatal("session remained usable after rejection before consuming layer data")
	}
}
