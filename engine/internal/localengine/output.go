//go:build linux || darwin

package localengine

import (
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"sync"
	"time"
)

// outputListener bounds memory while preserving output for a slower reader.
// Once the channel fills, records spill to a private temporary file. The
// shared guest reader can continue handling control replies and other runs.
type outputListener struct {
	output chan record
	wake   chan struct{}
	abort  chan struct{}
	done   chan struct{}
	dir    string

	mu            sync.Mutex
	file          *os.File
	read, written int64
	closed        bool
	canceled      bool
	err           error
}

func newOutputListener(dir string) *outputListener {
	l := &outputListener{
		output: make(chan record, 64), wake: make(chan struct{}, 1),
		abort: make(chan struct{}), done: make(chan struct{}), dir: dir,
	}
	go l.drain()
	return l
}

func (l *outputListener) signal() {
	select {
	case l.wake <- struct{}{}:
	default:
	}
}

func (l *outputListener) enqueue(entry record) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed || l.canceled {
		return l.err
	}
	if l.file == nil {
		select {
		case l.output <- entry:
			return nil
		default:
		}
		l.file, l.err = os.CreateTemp(l.dir, ".output-*")
	}
	if l.err == nil {
		var header [13]byte
		header[0] = entry.stream
		if !entry.at.IsZero() {
			binary.BigEndian.PutUint64(header[1:9], uint64(entry.at.UnixNano()))
		}
		binary.BigEndian.PutUint32(header[9:], uint32(len(entry.data)))
		_, l.err = l.file.Write(header[:])
		if l.err == nil {
			_, l.err = l.file.Write(entry.data)
		}
		if l.err == nil {
			l.written += int64(len(header) + len(entry.data))
		}
	}
	if l.err != nil {
		l.err = fmt.Errorf("buffer container output: %w", l.err)
		l.closed = true
	}
	l.signal()
	return l.err
}

func (l *outputListener) finish() {
	l.mu.Lock()
	l.closed = true
	l.mu.Unlock()
	l.signal()
}

func (l *outputListener) cancel() {
	l.mu.Lock()
	if !l.canceled {
		l.canceled = true
		close(l.abort)
	}
	l.mu.Unlock()
}

func (l *outputListener) failure() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.err
}

func (l *outputListener) drain() {
	defer func() {
		l.mu.Lock()
		l.closed = true
		if l.file != nil {
			name := l.file.Name()
			_ = l.file.Close()
			_ = os.Remove(name)
		}
		close(l.output)
		l.mu.Unlock()
		close(l.done)
	}()
	for {
		l.mu.Lock()
		if l.canceled {
			l.mu.Unlock()
			return
		}
		if l.read == l.written {
			if l.closed {
				l.mu.Unlock()
				return
			}
			if l.file != nil && l.written != 0 {
				l.err = l.file.Truncate(0)
				if l.err == nil {
					_, l.err = l.file.Seek(0, io.SeekStart)
				}
				if l.err != nil {
					l.mu.Unlock()
					return
				}
				l.read, l.written = 0, 0
			}
			l.mu.Unlock()
			select {
			case <-l.wake:
			case <-l.abort:
				return
			}
			continue
		}
		var header [13]byte
		_, err := l.file.ReadAt(header[:], l.read)
		var entry record
		if err == nil {
			entry.stream = header[0]
			if nanos := binary.BigEndian.Uint64(header[1:9]); nanos != 0 {
				entry.at = time.Unix(0, int64(nanos)).UTC()
			}
			entry.data = make([]byte, binary.BigEndian.Uint32(header[9:]))
			_, err = l.file.ReadAt(entry.data, l.read+int64(len(header)))
		}
		if err != nil {
			l.err = fmt.Errorf("read buffered container output: %w", err)
			l.mu.Unlock()
			return
		}
		l.mu.Unlock()
		select {
		case l.output <- entry:
		case <-l.abort:
			return
		}
		l.mu.Lock()
		l.read += int64(len(header) + len(entry.data))
		l.mu.Unlock()
	}
}
