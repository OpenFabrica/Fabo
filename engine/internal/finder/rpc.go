// Package finder serves the running container file view over read-only NFSv3.
package finder

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"sync"
	"unicode/utf8"

	"github.com/OpenFabrica/Fabo/engine/internal/guestwire"
)

const (
	programNFS   = 100003
	programMount = 100005
	maxRecord    = 64 * 1024
	lastFragment = 1 << 31
)

// Source provides live metadata and bytes for running containers. Its
// container map is a snapshot of names to full container IDs.
type Source interface {
	FileContainers() map[string]string
	FileStat(context.Context, string, string) (guestwire.FileInfo, error)
	FileList(context.Context, string, string) ([]guestwire.FileInfo, error)
	FileRead(context.Context, guestwire.FileQuery) ([]byte, error)
}

// Serve owns listener until ctx is canceled or accepting a connection fails.
// Only connections admitted by allow may send RPC calls.
func Serve(ctx context.Context, listener *net.UnixListener, source Source, allow func(*net.UnixConn) bool) error {
	handles := newHandles()
	ctx, cancel := context.WithCancel(ctx)
	var clients sync.WaitGroup
	stop := context.AfterFunc(ctx, func() { _ = listener.Close() })
	defer func() {
		cancel()
		clients.Wait()
		stop()
	}()
	for {
		conn, err := listener.AcceptUnix()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		if !allow(conn) {
			_ = conn.Close()
			continue
		}
		clients.Go(func() {
			defer conn.Close()
			stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
			defer stop()
			server := server{source: source, handles: handles}
			_ = server.serve(ctx, conn)
		})
	}
}

func (s *server) serve(ctx context.Context, conn io.ReadWriter) error {
	record := make([]byte, 0, 4096)
	var mark [4]byte
	for ctx.Err() == nil {
		record = record[:0]
		for {
			if _, err := io.ReadFull(conn, mark[:]); err != nil {
				return err
			}
			fragment := binary.BigEndian.Uint32(mark[:])
			n := int(fragment &^ lastFragment)
			if n > maxRecord-len(record) || (n == 0 && fragment&lastFragment == 0) {
				return fmt.Errorf("invalid Finder RPC fragment")
			}
			start := len(record)
			if cap(record) < start+n {
				record = append(record, make([]byte, n)...)
			} else {
				record = record[:start+n]
			}
			if _, err := io.ReadFull(conn, record[start:]); err != nil {
				return err
			}
			if fragment&lastFragment != 0 {
				break
			}
		}
		reply := s.call(ctx, record)
		if reply == nil {
			continue
		}
		binary.BigEndian.PutUint32(mark[:], uint32(len(reply))|lastFragment)
		if _, err := conn.Write(mark[:]); err != nil {
			return err
		}
		if _, err := conn.Write(reply); err != nil {
			return err
		}
	}
	return ctx.Err()
}

func (s *server) call(ctx context.Context, record []byte) []byte {
	a := xdrArgs{data: record}
	xid, kind := a.u32(), a.u32()
	if a.bad || kind != 0 {
		return nil
	}
	var out xdrReply
	out.words(xid, 1)
	if a.u32() != 2 {
		out.words(1, 0, 2, 2) // MSG_DENIED, RPC_MISMATCH
		return out
	}
	program, version, procedure := a.u32(), a.u32(), a.u32()
	for range 2 {
		a.u32()
		a.opaque() // credential, then verifier
	}
	out.words(0, 0, 0) // MSG_ACCEPTED, AUTH_NONE verifier
	switch {
	case a.bad:
		out.words(4)
	case program != programNFS && program != programMount:
		out.words(1) // PROG_UNAVAIL
	case version != 3:
		out.words(2, 3, 3)
	default:
		header := len(out)
		out.words(0)
		var supported bool
		if program == programNFS {
			supported = s.nfs(ctx, procedure, &a, &out)
		} else {
			supported = s.mount(procedure, &a, &out)
		}
		if a.bad {
			out = out[:header]
			out.words(4) // GARBAGE_ARGS
		} else if !supported {
			out = out[:header]
			out.words(3) // PROC_UNAVAIL
		}
	}
	return out
}

func (s *server) mount(procedure uint32, a *xdrArgs, out *xdrReply) bool {
	switch procedure {
	case 0, 3, 4: // NULL, UMNT, UMNTALL
	case 1:
		if a.text() != "/" {
			out.words(nfsNoEntry)
			break
		}
		out.words(nfsOK)
		out.opaque(rootHandle[:])
		out.words(1, 1) // one auth flavor, AUTH_SYS
	case 5:
		out.words(1)
		out.opaque([]byte("/"))
		out.words(0, 0)
	default:
		return false
	}
	return true
}

type xdrArgs struct {
	data []byte
	bad  bool
}

func (a *xdrArgs) fixed(n int) []byte {
	if a.bad || n < 0 || n > len(a.data) {
		a.bad = true
		return nil
	}
	v := a.data[:n]
	a.data = a.data[n:]
	return v
}

func (a *xdrArgs) u32() uint32 {
	if b := a.fixed(4); b != nil {
		return binary.BigEndian.Uint32(b)
	}
	return 0
}

func (a *xdrArgs) u64() uint64 {
	return uint64(a.u32())<<32 | uint64(a.u32())
}

func (a *xdrArgs) opaque() []byte {
	n := int(a.u32())
	if padded := a.fixed((n + 3) &^ 3); padded != nil {
		return padded[:n]
	}
	return nil
}

func (a *xdrArgs) text() string {
	data := a.opaque()
	if !utf8.Valid(data) {
		a.bad = true
		return ""
	}
	return string(data)
}

type xdrReply []byte

func (out *xdrReply) words(values ...uint32) {
	for _, n := range values {
		*out = binary.BigEndian.AppendUint32(*out, n)
	}
}

func (out *xdrReply) wide(values ...uint64) {
	for _, n := range values {
		*out = binary.BigEndian.AppendUint64(*out, n)
	}
}

func (out *xdrReply) boolean(value bool) {
	if value {
		out.words(1)
	} else {
		out.words(0)
	}
}

func (out *xdrReply) opaque(value []byte) {
	out.words(uint32(len(value)))
	*out = append(*out, value...)
	*out = append(*out, make([]byte, -len(value)&3)...)
}
