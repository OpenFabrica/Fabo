package guestwire

import (
	"encoding/binary"
	"fmt"
	"io"
)

const (
	CopyPort             = 1028
	CopyMessage     Kind = 32
	CopyStatMessage Kind = 33
	CopyDoneMessage Kind = 34

	CopyIn     uint8 = 1
	CopyOut    uint8 = 2
	CopyStat   uint8 = 3
	CopyDiff   uint8 = 4
	CopyExport uint8 = 5
	CopyCommit uint8 = 6

	CopyKeepOwners uint32 = 1
	CopyNoReplace  uint32 = 2
	CopyChunked    uint32 = 4
	copyChunkSize         = 256 << 10
)

type FilesystemChange struct {
	Path string
	Kind int
}

type CopySpec struct {
	Op       uint8
	ID, Path string
	Flags    uint32
	Layers   []string
}

func (s CopySpec) Frame() (Frame, error) {
	var e encoder
	e.add(s.Op)
	e.text(s.ID)
	e.text(s.Path)
	e.u32(s.Flags)
	e.list(s.Layers)
	return Frame{CopyMessage, e.payload}, e.err
}

func DecodeCopy(f Frame) (CopySpec, error) {
	if f.Kind != CopyMessage {
		return CopySpec{}, fmt.Errorf("expected a copy request, got kind %d", f.Kind)
	}
	d := runDecoder{data: f.Payload}
	s := CopySpec{Op: uint8(d.number(1)), ID: d.text(), Path: d.text(), Flags: uint32(d.number(4)), Layers: d.list()}
	if d.err != nil {
		return CopySpec{}, d.err
	}
	if len(d.data) != 0 {
		return CopySpec{}, fmt.Errorf("trailing bytes in copy request")
	}
	return s, nil
}

type ArchiveStat struct {
	Name, Link string
	Mode       uint32
	Size       uint64
	Sec        int64
	Nsec       uint32
	Inode      uint64
}

func (s ArchiveStat) Frame() (Frame, error) {
	var e encoder
	e.text(s.Name)
	e.u32(s.Mode)
	e.u64(s.Size)
	e.u64(uint64(s.Sec))
	e.u32(s.Nsec)
	e.u64(s.Inode)
	e.text(s.Link)
	return Frame{CopyStatMessage, e.payload}, e.err
}

func DecodeArchiveStat(f Frame) (ArchiveStat, error) {
	if err := expect(f, CopyStatMessage); err != nil {
		return ArchiveStat{}, err
	}
	d := runDecoder{data: f.Payload}
	s := ArchiveStat{Name: d.text(), Mode: uint32(d.number(4)), Size: d.number(8),
		Sec: int64(d.number(8)), Nsec: uint32(d.number(4)), Inode: d.number(8), Link: d.text()}
	if d.err != nil {
		return ArchiveStat{}, d.err
	}
	if len(d.data) != 0 {
		return ArchiveStat{}, fmt.Errorf("trailing bytes in copy stat")
	}
	return s, nil
}

// WriteCopyChunks terminates an upload with an empty little-endian chunk.
// The connection stays open so the guest can report the result.
func WriteCopyChunks(dst io.Writer, src io.Reader) error {
	buf := make([]byte, 4+copyChunkSize)
	for {
		n, err := src.Read(buf[4:])
		if n > 0 {
			binary.LittleEndian.PutUint32(buf[:4], uint32(n))
			if err := write(dst, buf[:4+n]); err != nil {
				return err
			}
		}
		if err == io.EOF {
			return write(dst, []byte{0, 0, 0, 0})
		}
		if err != nil {
			return err
		}
	}
}

// CopyChunkWriter frames a streaming archive. Close marks successful EOF.
type CopyChunkWriter struct {
	Target io.Writer
}

func (w CopyChunkWriter) Write(p []byte) (int, error) {
	var header [4]byte
	written := 0
	for len(p) > 0 {
		size := min(len(p), copyChunkSize)
		binary.LittleEndian.PutUint32(header[:], uint32(size))
		if err := write(w.Target, header[:]); err != nil {
			return written, err
		}
		if err := write(w.Target, p[:size]); err != nil {
			return written, err
		}
		written += size
		p = p[size:]
	}
	return written, nil
}

func (w CopyChunkWriter) Close() error {
	return write(w.Target, []byte{0, 0, 0, 0})
}

type CopyChunkReader struct {
	Source io.Reader
	left   uint32
	done   bool
}

func (r *CopyChunkReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if r.done {
		return 0, io.EOF
	}
	if r.left == 0 {
		var header [4]byte
		if _, err := io.ReadFull(r.Source, header[:]); err != nil {
			if err == io.EOF {
				err = io.ErrUnexpectedEOF
			}
			return 0, err
		}
		r.left = binary.LittleEndian.Uint32(header[:])
		if r.left == 0 {
			r.done = true
			return 0, io.EOF
		}
	}
	n, err := r.Source.Read(p[:min(uint32(len(p)), r.left)])
	r.left -= uint32(n)
	if err == io.EOF {
		err = io.ErrUnexpectedEOF
	}
	return n, err
}
