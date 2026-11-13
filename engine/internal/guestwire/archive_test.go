package guestwire

import (
	"bytes"
	"errors"
	"io"
	"testing"
)

func TestCopyChunksEndBeforeReply(t *testing.T) {
	source := bytes.Repeat([]byte("file"), copyChunkSize)
	var wire bytes.Buffer
	if err := WriteCopyChunks(&wire, bytes.NewReader(source)); err != nil {
		t.Fatal(err)
	}
	if err := WriteFrame(&wire, Frame{Kind: CopyDoneMessage}); err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(&CopyChunkReader{Source: &wire})
	if err != nil || !bytes.Equal(got, source) {
		t.Fatalf("archive upload: got %d bytes, %v", len(got), err)
	}
	if reply, err := ReadFrame(&wire, nil); err != nil || reply.Kind != CopyDoneMessage {
		t.Fatalf("copy reply lost: %+v, %v", reply, err)
	}
}

func TestCopyChunksRejectMissingEndMarker(t *testing.T) {
	var wire bytes.Buffer
	if err := WriteCopyChunks(&wire, bytes.NewBufferString("content")); err != nil {
		t.Fatal(err)
	}
	wire.Truncate(wire.Len() - 4)
	if _, err := io.Copy(io.Discard, &CopyChunkReader{Source: &wire}); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("truncated upload reported %v", err)
	}
}
