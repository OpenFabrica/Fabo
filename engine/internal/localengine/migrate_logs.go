//go:build linux || darwin

package localengine

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"
)

func readRustLogs(ctx context.Context, source *os.Root, c *container) error {
	dir := path.Join("containers", c.id)
	entries, err := fs.ReadDir(source.FS(), dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	type logfile struct {
		name     string
		rotation int
	}
	var files []logfile
	for _, entry := range entries {
		if entry.Name() == "container.log" {
			files = append(files, logfile{entry.Name(), 0})
		} else if suffix, ok := strings.CutPrefix(entry.Name(), "container.log."); ok {
			if n, err := strconv.Atoi(suffix); err == nil && n > 0 {
				files = append(files, logfile{entry.Name(), n})
			}
		}
	}
	slices.SortFunc(files, func(a, b logfile) int { return a.rotation - b.rotation })
	var records []record
	left := containerLogLimit
	for _, file := range files {
		if left <= 0 {
			break
		}
		f, err := source.Open(path.Join(dir, file.name))
		if err != nil {
			return err
		}
		err = readRustLogTail(ctx, f, &records, &left)
		if err := errors.Join(err, f.Close()); err != nil {
			return err
		}
	}
	for _, entry := range slices.Backward(records) {
		c.appendLog(entry)
	}
	return nil
}

// Rust logs repeat each length at the end, so we can read the most
// recent records without copying the entire set of rotated logs.
func readRustLogTail(ctx context.Context, f *os.File, records *[]record, remaining *int) error {
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("source log is not a regular file")
	}
	for end := info.Size(); end > 0 && *remaining > 0; {
		if err := ctx.Err(); err != nil {
			return err
		}
		var word [4]byte
		if end < 8 {
			return io.ErrUnexpectedEOF
		}
		if _, err := f.ReadAt(word[:], end-4); err != nil {
			return err
		}
		length := int64(binary.BigEndian.Uint32(word[:]))
		if length < 9 || length > (16<<10)+9 || length+8 > end {
			return fmt.Errorf("invalid source log record length")
		}
		start := end - length - 8
		buf := make([]byte, 4+length)
		if _, err := f.ReadAt(buf, start); err != nil {
			return err
		}
		if binary.BigEndian.Uint32(buf) != uint32(length) || (buf[4] != 1 && buf[4] != 2) {
			return fmt.Errorf("invalid source log record")
		}
		nanos := int64(binary.BigEndian.Uint64(buf[5:13]))
		data := buf[13:]
		if len(data) != 0 {
			*records = append(*records, record{stream: buf[4], data: data, at: time.Unix(0, nanos).UTC()})
			*remaining -= len(data)
		}
		end = start
	}
	return nil
}
