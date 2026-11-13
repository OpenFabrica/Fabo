package guestwire

import (
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"path"
	"strings"
)

const (
	FileStatRequest Kind = 24
	FileStatReply   Kind = 25
	FileListRequest Kind = 26
	FileListReply   Kind = 27
	FileReadRequest Kind = 28
	FileReadReply   Kind = 29

	MaxFileRead    = 512 * 1024
	MaxFileEntries = 5000
)

// FileInfo is the Linux metadata returned by the guest's read-only file view.
type FileInfo struct {
	Name    string
	Mode    uint32
	Size    uint64
	Mtime   int64
	MtimeNS uint32
	Inode   uint64
}

func (f FileInfo) encode(e *encoder) {
	e.text(f.Name)
	e.u32(f.Mode)
	e.u64(f.Size)
	e.u64(uint64(f.Mtime))
	e.u32(f.MtimeNS)
	e.u64(f.Inode)
}

func decodeFileInfo(d *runDecoder) FileInfo {
	return FileInfo{
		Name: d.text(), Mode: uint32(d.number(4)), Size: d.number(8),
		Mtime: int64(d.number(8)), MtimeNS: uint32(d.number(4)), Inode: d.number(8),
	}
}

// FileQuery names a path rooted in a running container. Offset and Count
// apply only to regular-file reads.
type FileQuery struct {
	ID, Path string
	Offset   uint64
	Count    uint32
}

func (q FileQuery) valid() bool {
	return path.IsAbs(q.Path) && len(q.Path) < 4096 && !strings.ContainsRune(q.Path, 0) &&
		q.Offset <= math.MaxInt64 && q.Count <= MaxFileRead
}

func (q FileQuery) frame(kind Kind) (Frame, error) {
	if !q.valid() {
		return Frame{}, fmt.Errorf("invalid container file query")
	}
	var e encoder
	e.text(q.ID)
	e.text(q.Path)
	if kind == FileReadRequest {
		e.u64(q.Offset)
		e.u32(q.Count)
	}
	return Frame{Kind: kind, Payload: e.payload}, e.err
}

func DecodeFileQuery(f Frame) (FileQuery, error) {
	d := runDecoder{data: f.Payload}
	q := FileQuery{ID: d.text(), Path: d.text()}
	if f.Kind == FileReadRequest {
		q.Offset, q.Count = d.number(8), uint32(d.number(4))
	}
	if d.err != nil || len(d.data) != 0 || !q.valid() {
		return FileQuery{}, fmt.Errorf("invalid container file query")
	}
	return q, nil
}

func (f FileInfo) Frame() (Frame, error) {
	var e encoder
	f.encode(&e)
	return Frame{Kind: FileStatReply, Payload: e.payload}, e.err
}

func FileListFrame(entries []FileInfo) (Frame, error) {
	var e encoder
	e.u32(0)
	var count uint32
	for _, entry := range entries {
		// Entries carry 36 bytes of fixed fields and their UTF-8 name.
		if count == MaxFileEntries || len(e.payload)+36+len(entry.Name) > MaxFrame-headerSize {
			break
		}
		entry.encode(&e)
		count++
	}
	binary.LittleEndian.PutUint32(e.payload, count)
	return Frame{Kind: FileListReply, Payload: e.payload}, e.err
}

func FileDataFrame(data []byte) Frame {
	return Frame{Kind: FileReadReply, Payload: append(binary.LittleEndian.AppendUint32(nil, uint32(len(data))), data...)}
}

func (s *Session) FileStat(ctx context.Context, id, path string) (FileInfo, error) {
	frame, err := s.queryFile(ctx, FileQuery{ID: id, Path: path}, FileStatRequest, FileStatReply)
	if err != nil {
		return FileInfo{}, err
	}
	d := runDecoder{data: frame.Payload}
	entry := decodeFileInfo(&d)
	if d.err != nil || len(d.data) != 0 {
		return FileInfo{}, fmt.Errorf("invalid guest file metadata")
	}
	return entry, nil
}

func (s *Session) FileList(ctx context.Context, id, path string) ([]FileInfo, error) {
	frame, err := s.queryFile(ctx, FileQuery{ID: id, Path: path}, FileListRequest, FileListReply)
	if err != nil {
		return nil, err
	}
	d := runDecoder{data: frame.Payload}
	count := d.number(4)
	if count > MaxFileEntries || count > uint64(len(d.data)/36) {
		return nil, fmt.Errorf("invalid guest directory listing")
	}
	entries := make([]FileInfo, count)
	for i := range entries {
		entries[i] = decodeFileInfo(&d)
	}
	if d.err != nil || len(d.data) != 0 {
		return nil, fmt.Errorf("invalid guest directory listing")
	}
	return entries, nil
}

func (s *Session) FileRead(ctx context.Context, q FileQuery) ([]byte, error) {
	frame, err := s.queryFile(ctx, q, FileReadRequest, FileReadReply)
	if err != nil {
		return nil, err
	}
	d := runDecoder{data: frame.Payload}
	size := d.number(4)
	if size > uint64(q.Count) || size != uint64(len(d.data)) || d.err != nil {
		return nil, fmt.Errorf("invalid guest file data")
	}
	return d.data, nil
}

func (s *Session) queryFile(ctx context.Context, q FileQuery, request, reply Kind) (Frame, error) {
	frame, err := q.frame(request)
	if err != nil {
		return Frame{}, err
	}
	return s.request(ctx, frame, reply, "", nil, 0)
}
