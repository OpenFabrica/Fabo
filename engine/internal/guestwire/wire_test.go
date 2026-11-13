package guestwire

import (
	"bytes"
	"encoding/hex"
	"errors"
	"io"
	"testing"
)

func TestHelloMatchesRustWireLayout(t *testing.T) {
	hello := Hello{Version: Version, InitStartNS: 1, ReadyNS: 2, MemoryTotalKiB: 3, MemoryAvailableKiB: 4}
	var buf bytes.Buffer
	if err := WriteFrame(&buf, hello.Frame()); err != nil {
		t.Fatal(err)
	}
	const want = "24000000010004000100000000000000020000000000000003000000000000000400000000000000"
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
