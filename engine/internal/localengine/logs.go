//go:build linux || darwin

package localengine

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/OpenFabrica/FabriBay/engine/internal/guestwire"
)

const containerLogLimit = 1 << 20

func (c *container) appendLog(entry record) {
	if n := len(entry.data); n > containerLogLimit {
		entry.data = slices.Clone(entry.data[n-containerLogLimit:])
	}
	for c.logBytes+len(entry.data) > containerLogLimit {
		c.logBytes -= len(c.logs[0].data)
		c.logs[0] = record{}
		c.logs = c.logs[1:]
	}
	c.logs = append(c.logs, entry)
	c.logBytes += len(entry.data)
}

func (e *Engine) logsHTTP(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if !flagSet(q, "stdout") && !flagSet(q, "stderr") {
		writeError(w, fmt.Errorf("%w: select at least one log stream", errInvalid))
		return
	}
	since, err := parseLogTime(q.Get("since"))
	if err != nil {
		writeError(w, fmt.Errorf("%w: invalid since timestamp", errInvalid))
		return
	}
	until, err := parseLogTime(q.Get("until"))
	if err != nil {
		writeError(w, fmt.Errorf("%w: invalid until timestamp", errInvalid))
		return
	}
	tail := -1
	if value := q.Get("tail"); value != "" && value != "all" {
		var err error
		tail, err = strconv.Atoi(value)
		if err != nil || tail < 0 {
			tail = -1 // Docker treats an invalid tail as all.
		}
	}
	e.mu.Lock()
	c, err := e.find(r.PathValue("id"))
	if err != nil {
		e.mu.Unlock()
		writeError(w, err)
		return
	}
	logs := slices.Clone(c.logs)
	tty := c.run.Flags&guestwire.RunTTY != 0
	var listener *outputListener
	if flagSet(q, "follow") && (until.IsZero() || time.Now().Before(until)) &&
		(c.state == "running" || c.state == "starting") {
		// Snapshot and registration share the same lock as guest output.
		// Each record therefore belongs to exactly one side of this handoff.
		dir := ""
		if e.state != nil {
			dir = e.state.dir
		}
		listener = c.listenOutput(dir)
	}
	e.mu.Unlock()
	if listener != nil {
		defer func() {
			e.mu.Lock()
			delete(c.listeners, listener)
			e.mu.Unlock()
			listener.cancel()
		}()
	}

	contentType := "application/vnd.docker.multiplexed-stream"
	if tty {
		contentType = "application/vnd.docker.raw-stream"
	}
	w.Header().Set("Content-Type", contentType)
	controller := http.NewResponseController(w)
	defer controller.SetWriteDeadline(time.Time{})
	_ = controller.SetWriteDeadline(time.Now().Add(15 * time.Second))
	inRange := func(entry record) bool {
		if entry.at.IsZero() {
			return since.IsZero() && until.IsZero()
		}
		return (since.IsZero() || !entry.at.Before(since)) && (until.IsZero() || !entry.at.After(until))
	}
	stamps := flagSet(q, "timestamps")
	var stdoutContinued, stderrContinued bool
	write := func(entry record) error {
		if stamps {
			continued := &stdoutContinued
			if !tty && entry.stream == 2 {
				continued = &stderrContinued
			}
			entry.data = timestampLog(entry, continued)
		}
		return writeSelected(w, q, entry, tty)
	}
	logs = slices.DeleteFunc(logs, func(entry record) bool { return !inRange(entry) })
	for _, entry := range selectLogTail(logs, q, tty, tail) {
		if err := write(entry); err != nil {
			return
		}
	}
	if listener == nil || controller.Flush() != nil {
		return
	}
	_ = controller.SetWriteDeadline(time.Time{})
	ctx := r.Context()
	if !until.IsZero() {
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, until)
		defer cancel()
	}
	for {
		select {
		case <-ctx.Done():
			return
		case entry, ok := <-listener.output:
			if !ok {
				if listener.failure() != nil {
					// A truncated follow must not look like a successful EOF.
					panic(http.ErrAbortHandler)
				}
				return
			}
			if !inRange(entry) {
				continue
			}
			_ = controller.SetWriteDeadline(time.Now().Add(15 * time.Second))
			if write(entry) != nil || controller.Flush() != nil {
				return
			}
			_ = controller.SetWriteDeadline(time.Time{})
		}
	}
}

// Docker CLI converts relative or calendar input to seconds[.nanoseconds].
// Direct API clients can also pass RFC3339 timestamps.
func parseLogTime(value string) (time.Time, error) {
	value = strings.TrimSpace(value)
	if value == "" || value == "0" {
		return time.Time{}, nil
	}
	whole, frac, hasFraction := strings.Cut(value, ".")
	seconds, err := strconv.ParseInt(whole, 10, 64)
	if err != nil {
		return time.Parse(time.RFC3339Nano, value)
	}
	var nanos int64
	if hasFraction {
		if len(frac) == 0 || len(frac) > 9 || strings.ContainsAny(frac, "+-") {
			return time.Time{}, fmt.Errorf("invalid fractional seconds")
		}
		nanos, err = strconv.ParseInt(frac+strings.Repeat("0", 9-len(frac)), 10, 64)
		if err != nil {
			return time.Time{}, err
		}
	}
	return time.Unix(seconds, nanos), nil
}

// Guest output arrives in chunks that can split a line. Add one prefix at
// each line start, carrying the continuation across the snapshot and live data.
func timestampLog(entry record, continued *bool) []byte {
	if entry.at.IsZero() {
		// Older records lack a timestamp. Preserve their bytes.
		*continued = len(entry.data) > 0 && entry.data[len(entry.data)-1] != '\n'
		return entry.data
	}
	stamp := entry.at.UTC().AppendFormat(nil, "2006-01-02T15:04:05.000000000Z")
	var result []byte
	for line := range bytes.SplitAfterSeq(entry.data, []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		if !*continued {
			result = append(result, stamp...)
			result = append(result, ' ')
		}
		result = append(result, line...)
		*continued = line[len(line)-1] != '\n'
	}
	return result
}

// A guest record is a read chunk, not necessarily a complete line. Locate
// the boundary in the selected byte stream before writing Docker frames.
func selectLogTail(logs []record, q url.Values, tty bool, tail int) []record {
	if tail == 0 {
		return nil
	}
	logs = slices.DeleteFunc(logs, func(entry record) bool {
		return len(entry.data) == 0 || (!tty && ((entry.stream == 1 && !flagSet(q, "stdout")) ||
			(entry.stream == 2 && !flagSet(q, "stderr"))))
	})
	if tail < 0 {
		return logs
	}
	trailing := true
	for i := len(logs) - 1; i >= 0; i-- {
		data := logs[i].data
		end := len(data)
		if trailing && data[end-1] == '\n' {
			end--
		}
		trailing = false
		for end > 0 {
			n := bytes.LastIndexByte(data[:end], '\n')
			if n < 0 {
				break
			}
			tail--
			if tail == 0 {
				if n+1 == len(data) {
					return logs[i+1:]
				}
				logs[i].data = data[n+1:]
				return logs[i:]
			}
			end = n
		}
	}
	return logs
}
