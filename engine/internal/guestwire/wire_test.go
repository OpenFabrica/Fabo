package guestwire

import (
	"bytes"
	"encoding/hex"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestHelloMatchesRustWireLayout(t *testing.T) {
	hello := Hello{Version: Version, InitStartNS: 1, ReadyNS: 2, MemoryTotalKiB: 3, MemoryAvailableKiB: 4}
	var buf bytes.Buffer
	if err := WriteFrame(&buf, hello.Frame()); err != nil {
		t.Fatal(err)
	}
	// The layout is the Rust guest's; the version field (bytes 6-7) holds
	// protocol 0.5 (major in the high byte), 05 00 on the wire.
	const want = "24000000010005000100000000000000020000000000000003000000000000000400000000000000"
	if got := hex.EncodeToString(buf.Bytes()); got != want {
		t.Fatalf("hello encoding %s, want %s", got, want)
	}
	if got, err := ReadHello(&buf); err != nil || got != hello {
		t.Fatalf("hello = %+v, %v", got, err)
	}
}

func TestHelloReportsRosettaRegistration(t *testing.T) {
	want := Hello{Version: Version, InitStartNS: 10, ReadyNS: 20, Rosetta: true}
	var buf bytes.Buffer
	if err := WriteFrame(&buf, want.Frame()); err != nil {
		t.Fatal(err)
	}
	if got, err := ReadHello(&buf); err != nil || got != want {
		t.Fatalf("hello = %+v, %v", got, err)
	}
}

func TestReadFrameInvalidLengths(t *testing.T) {
	for _, b := range [][]byte{
		{1, 0, 0, 0, 1, 0},
		{0, 0, 16, 0, 1, 0},
	} {
		if _, err := ReadFrame(bytes.NewReader(b), nil); err == nil {
			t.Fatalf("accepted invalid length %v", b)
		}
	}
	b := []byte{4, 0, 0, 0, 1, 0, 1}
	if _, err := ReadFrame(bytes.NewReader(b), nil); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("truncated frame = %v", err)
	}
}

func TestReadHelloRejectsOldVersion(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteFrame(&buf, (Hello{Version: Version - 1}).Frame()); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadHello(&buf); err == nil {
		t.Fatal("accepted old protocol")
	}
}

// A guest of protocol 0.4 knows no Settled event, which this host requires
// before exec: it is refused at Hello instead of accepted and waited on.
func TestHelloRefusesProtocol04(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteFrame(&buf, Hello{Version: 4}.Frame()); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadHello(&buf); err == nil || !strings.Contains(err.Error(), "guest protocol 0.4, expected 0.5") {
		t.Fatalf("protocol 0.4 guest: %v", err)
	}
	buf.Reset()
	if err := WriteFrame(&buf, Hello{Version: Version}.Frame()); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadHello(&buf); err != nil {
		t.Fatalf("current guest refused: %v", err)
	}
}

// Versions read as major.minor, the major in the high byte.
func TestProtocolString(t *testing.T) {
	for version, want := range map[uint16]string{4: "0.4", Version: "0.5", 1<<8 | 2: "1.2"} {
		if got := ProtocolString(version); got != want {
			t.Fatalf("ProtocolString(%d) = %q, want %q", version, got, want)
		}
	}
}
