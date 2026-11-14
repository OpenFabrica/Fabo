package finder

import (
	"context"
	"encoding/binary"
	"errors"
	"hash/fnv"
	"io/fs"
	"maps"
	"math"
	"math/rand/v2"
	"os"
	"path"
	"slices"
	"strings"
	"sync"
	"syscall"

	"github.com/OpenFabrica/FabriBay/engine/internal/guestwire"
)

const (
	nfsOK          = 0
	nfsNoEntry     = 2
	nfsIO          = 5
	nfsAccess      = 13
	nfsNotDir      = 20
	nfsIsDir       = 21
	nfsReadOnly    = 30
	nfsStale       = 70
	nfsBadHandle   = 10001
	nfsUnsupported = 10004
	nfsTooSmall    = 10005

	maxHandles = 50_000
	fsid       = 0x4b495249
)

type node struct {
	id   string
	path string
}

type fileHandle [16]byte

var rootHandle = encodeHandle(math.MaxUint64, 0)

func encodeHandle(generation, index uint64) fileHandle {
	var handle fileHandle
	binary.BigEndian.PutUint64(handle[:8], generation)
	binary.BigEndian.PutUint64(handle[8:], index)
	return handle
}

func (handle fileHandle) fileID() uint64 {
	return binary.BigEndian.Uint64(handle[8:]) + 1
}

type handles struct {
	mu         sync.Mutex
	generation uint64
	nodes      []node
	indices    map[node]uint64
}

func newHandles() *handles {
	return &handles{generation: rand.Uint64N(math.MaxUint64), indices: make(map[node]uint64)}
}

func (h *handles) get(n node) fileHandle {
	if n.id == "" {
		return rootHandle
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	index := h.indices[n]
	if index == 0 {
		if len(h.nodes) == maxHandles {
			h.generation++
			h.nodes = h.nodes[:0]
			clear(h.indices)
		}
		h.nodes = append(h.nodes, n)
		index = uint64(len(h.nodes))
		h.indices[n] = index
	}
	return encodeHandle(h.generation, index)
}

func (h *handles) lookup(raw []byte) (node, uint32) {
	if len(raw) != len(rootHandle) {
		return node{}, nfsBadHandle
	}
	if fileHandle(raw) == rootHandle {
		return node{}, nfsOK
	}
	generation := binary.BigEndian.Uint64(raw[:8])
	index := binary.BigEndian.Uint64(raw[8:])
	h.mu.Lock()
	defer h.mu.Unlock()
	if generation != h.generation || index == 0 || index > uint64(len(h.nodes)) {
		return node{}, nfsStale
	}
	return h.nodes[index-1], nfsOK
}

type listingEntry struct {
	node node
	file guestwire.FileInfo
}

type server struct {
	source  Source
	handles *handles
	listed  fileHandle
	listing []listingEntry
}

func (s *server) nfs(ctx context.Context, procedure uint32, a *xdrArgs, out *xdrReply) bool {
	switch procedure {
	case 0: // NULL
		return true
	case 2, 7, 8, 9, 10, 11, 12, 13, 21: // mutation, with one missing WCC pair
		out.words(nfsReadOnly, 0, 0)
		return true
	case 14: // RENAME, with two missing WCC pairs
		out.words(nfsReadOnly, 0, 0, 0, 0)
		return true
	case 15: // LINK, with missing attributes and WCC
		out.words(nfsReadOnly, 0, 0, 0)
		return true
	case 1, 3, 4, 5, 6, 16, 17, 18, 19, 20:
	default:
		return false
	}
	raw := a.opaque()
	n, status := s.handles.lookup(raw)
	if status != nfsOK {
		out.words(status)
		if procedure != 1 {
			out.words(0) // no post-op attributes
		}
		return true
	}
	handle := fileHandle(raw)
	switch procedure {
	case 3:
		name := a.text()
		if !a.bad {
			s.lookup(ctx, handle, n, name, out)
		}
	case 5: // READLINK, symlinks are resolved by the guest
		out.words(nfsUnsupported, 0)
	case 6:
		offset, count := a.u64(), a.u32()
		if !a.bad {
			s.read(ctx, n, offset, min(count, guestwire.MaxFileRead), out)
		}
	case 16, 17:
		cookie := a.u64()
		a.fixed(8) // cookie verifier
		count := a.u32()
		if procedure == 17 {
			count = a.u32()
		}
		if !a.bad {
			s.readdir(ctx, handle, n, cookie, count, procedure == 17, out)
		}
	default:
		var want uint32
		if procedure == 4 {
			want = a.u32()
		}
		if a.bad {
			return true
		}
		info, status := s.stat(ctx, n)
		out.words(status)
		if status != nfsOK {
			if procedure != 1 {
				out.words(0)
			}
			return true
		}
		if procedure != 1 {
			out.words(1)
		}
		writeAttr(out, info, handle.fileID())
		switch procedure {
		case 4:
			out.words(want & 0x23) // READ, LOOKUP, EXECUTE
		case 18: // FSSTAT
			out.wide(1<<40, 0, 0, 1<<20, 0, 0)
			out.words(0)
		case 19: // FSINFO
			out.words(guestwire.MaxFileRead, guestwire.MaxFileRead, 4096, 0, 0, 4096, 64*1024)
			out.wide(math.MaxInt64)
			out.words(0, 1, 0x08) // nanosecond time resolution, FSF_HOMOGENEOUS
		case 20: // PATHCONF
			out.words(1, 255, 1, 1, 0, 1)
		}
	}
	return true
}

func (s *server) stat(ctx context.Context, n node) (guestwire.FileInfo, uint32) {
	if n.id != "" {
		info, err := s.source.FileStat(ctx, n.id, n.path)
		return info, nfsStatus(err)
	}
	containers := s.source.FileContainers()
	hash := fnv.New64a()
	for _, name := range slices.Sorted(maps.Keys(containers)) {
		_, _ = hash.Write([]byte(name))
		_, _ = hash.Write([]byte(containers[name]))
	}
	clock := hash.Sum64()
	return guestwire.FileInfo{Mode: 0o040755, Size: 4096, Mtime: 1_700_000_000 + int64(clock%1_000_000), MtimeNS: uint32(clock>>20) % 1_000_000_000}, nfsOK
}

func (s *server) lookup(ctx context.Context, parentHandle fileHandle, parent node, name string, out *xdrReply) {
	dir, status := s.stat(ctx, parent)
	if status != nfsOK {
		out.words(status, 0)
		return
	}
	child := parent
	info := dir
	switch {
	case dir.Mode&0o170000 != 0o040000:
		status = nfsNotDir
	case name == "." || (name == ".." && parent.id == ""):
	case name == "" || strings.ContainsAny(name, "/\x00"):
		status = nfsNoEntry
	case parent.id == "":
		id := s.source.FileContainers()[name]
		if id == "" {
			status = nfsNoEntry
		} else {
			child = node{id: id, path: "/"}
			info, status = s.stat(ctx, child)
		}
	default:
		child.path = path.Join(parent.path, name)
		info, status = s.stat(ctx, child)
	}
	out.words(status)
	if status == nfsOK {
		fh := s.handles.get(child)
		out.opaque(fh[:])
		out.words(1)
		writeAttr(out, info, fh.fileID())
	}
	out.words(1)
	writeAttr(out, dir, parentHandle.fileID())
}

func (s *server) read(ctx context.Context, n node, offset uint64, count uint32, out *xdrReply) {
	if n.id == "" {
		out.words(nfsIsDir, 0)
		return
	}
	bytes, err := s.source.FileRead(ctx, guestwire.FileQuery{ID: n.id, Path: n.path, Offset: offset, Count: count})
	out.words(nfsStatus(err), 0) // the open already fetched fresh attributes
	if err == nil {
		out.words(uint32(len(bytes)))
		out.boolean(len(bytes) < int(count))
		out.opaque(bytes)
	}
}

func (s *server) readdir(ctx context.Context, fh fileHandle, n node, cookie uint64, count uint32, plus bool, out *xdrReply) {
	dir, status := s.stat(ctx, n)
	if status != nfsOK {
		out.words(status, 0)
		return
	}
	if dir.Mode&0o170000 != 0o040000 {
		status = nfsNotDir
	}
	if status == nfsOK && (cookie == 0 || fh != s.listed) {
		s.listing, status = s.readListing(ctx, n)
		if status == nfsOK {
			s.listed = fh
		}
	}
	start := len(*out)
	out.words(status, 1)
	writeAttr(out, dir, fh.fileID())
	if status != nfsOK {
		return
	}
	out.wide(0)
	eof := true
	for index := cookie; index < uint64(len(s.listing)); index++ {
		entry := s.listing[index]
		child := s.handles.get(entry.node)
		var item xdrReply
		item.words(1)
		item.wide(child.fileID())
		item.opaque([]byte(entry.file.Name))
		item.wide(index + 1)
		if plus {
			item.boolean(n.id != "")
			if n.id != "" {
				writeAttr(&item, entry.file, child.fileID())
			}
			item.words(1)
			item.opaque(child[:])
		}
		if len(*out)-start+len(item)+8 > int(count) {
			if index == cookie {
				*out = (*out)[:start]
				out.words(nfsTooSmall, 1)
				writeAttr(out, dir, fh.fileID())
				return
			}
			eof = false
			break
		}
		*out = append(*out, item...)
	}
	out.words(0)
	out.boolean(eof)
}

func (s *server) readListing(ctx context.Context, dir node) ([]listingEntry, uint32) {
	if dir.id == "" {
		containers := s.source.FileContainers()
		entries := make([]listingEntry, 0, len(containers))
		for _, name := range slices.Sorted(maps.Keys(containers)) {
			entries = append(entries, listingEntry{node: node{id: containers[name], path: "/"}, file: guestwire.FileInfo{Name: name}})
		}
		return entries, nfsOK
	}
	files, err := s.source.FileList(ctx, dir.id, dir.path)
	if err != nil {
		return nil, nfsStatus(err)
	}
	entries := make([]listingEntry, 0, len(files))
	for _, file := range files {
		if file.Name == "" || file.Name == "." || file.Name == ".." || strings.ContainsAny(file.Name, "/\x00") {
			continue
		}
		entries = append(entries, listingEntry{node: node{id: dir.id, path: path.Join(dir.path, file.Name)}, file: file})
	}
	return entries, nfsOK
}

func writeAttr(out *xdrReply, info guestwire.FileInfo, fileID uint64) {
	kind := uint32(1) // regular
	mode := info.Mode&0o7555 | 0o444
	links := uint32(1)
	switch info.Mode & 0o170000 {
	case 0o040000:
		kind, links = 2, 2
		mode |= 0o111
	case 0o060000:
		kind = 3
	case 0o020000:
		kind = 4
	case 0o120000:
		kind = 5
	case 0o140000:
		kind = 6
	case 0o010000:
		kind = 7
	}
	out.words(kind, mode, links, uint32(os.Getuid()), uint32(os.Getgid()))
	out.wide(info.Size, (info.Size+4095)&^4095)
	out.words(0, 0) // device major/minor
	out.wide(fsid, fileID)
	for range 3 {
		out.words(uint32(max(0, info.Mtime)), info.MtimeNS)
	}
}

func nfsStatus(err error) uint32 {
	if err == nil {
		return nfsOK
	}
	message := err.Error()
	switch {
	case errors.Is(err, syscall.ESTALE) || strings.Contains(message, "not running"):
		return nfsStale
	case errors.Is(err, fs.ErrNotExist) || strings.Contains(message, "no such file or directory"):
		return nfsNoEntry
	case errors.Is(err, fs.ErrPermission) || strings.Contains(message, "permission denied"):
		return nfsAccess
	case errors.Is(err, syscall.ENOTDIR) || strings.Contains(message, "not a directory"):
		return nfsNotDir
	case errors.Is(err, syscall.EISDIR) || strings.Contains(message, "is a directory"):
		return nfsIsDir
	default:
		return nfsIO
	}
}
