package main

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// execStream owns one upgraded connection and its guest exec. Unlike ordinary
// Docker attach, Close terminates this exec: terminals and followed logs must
// not leave an invisible shell/kubectl in the container.
type execStream struct {
	io.ReadWriteCloser
	reader     io.Reader
	engine     engineClient
	id         string
	cancel     context.CancelFunc
	once       sync.Once
	done       chan struct{}
	cleanupErr error // published by closing done
	background bool
}

func (s *execStream) Read(p []byte) (int, error) { return s.reader.Read(p) }
func (s *execStream) Resize(cols, rows int) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return s.engine.request(ctx, http.MethodPost, fmt.Sprintf("/exec/%s/resize?h=%d&w=%d", url.PathEscape(s.id), rows, cols), nil, nil)
}
func (s *execStream) Close() error {
	s.once.Do(func() {
		if s.ReadWriteCloser != nil {
			_ = s.ReadWriteCloser.Close()
		}
		s.cancel()
		go func() {
			defer close(s.done)
			ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
			defer cancel()
			if s.background {
				ctx = context.WithValue(ctx, backgroundRead{}, true)
			}
			s.cleanupErr = s.engine.request(ctx, http.MethodPost, "/kiri/exec/"+url.PathEscape(s.id)+"/stop", nil, nil)
			// Removal of the container also removes its exec records.
			if failed, ok := s.cleanupErr.(*engineError); ok && failed.status == http.StatusNotFound && strings.Contains(strings.ToLower(failed.message), "no such exec") {
				s.cleanupErr = nil
			}
		}()
	})
	return nil
}

func (e engineClient) openExec(parent context.Context, id string, args []string, tty, stdin, background bool) (stream *execStream, err error) {
	ctx, cancel := context.WithCancel(parent)
	if background {
		ctx = context.WithValue(ctx, backgroundRead{}, true)
	}
	timer := time.AfterFunc(15*time.Second, cancel)
	defer timer.Stop()
	var capabilities struct{ KiriExecStop bool }
	if err = e.get(ctx, "/info", &capabilities); err != nil {
		cancel()
		return nil, err
	}
	if !capabilities.KiriExecStop {
		cancel()
		return nil, fmt.Errorf("this engine cannot close desktop exec sessions yet; restart the engine from the updated app")
	}
	// Bound startup only. A quiet terminal/log stream has no lifetime timeout.

	defer func() {
		if err != nil {
			cancel()
		}
	}()
	var created struct{ ID string }
	spec := map[string]any{"Cmd": args, "AttachStdout": true, "AttachStderr": true, "AttachStdin": stdin, "Tty": tty, "Env": []string{"TERM=xterm-256color", "COLORTERM=truecolor"}, "ConsoleSize": []int{24, 80}}
	if err = e.request(ctx, http.MethodPost, containerPath(id)+"/exec", spec, &created); err != nil {
		return nil, err
	}
	stream = &execStream{engine: e, id: created.ID, cancel: cancel, done: make(chan struct{}), background: background}
	defer func() {
		if err != nil {
			_ = stream.Close()
		}
	}()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://localhost/exec/"+url.PathEscape(created.ID)+"/start", strings.NewReader(fmt.Sprintf(`{"Detach":false,"Tty":%t}`, tty)))
	if err != nil {
		return stream, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "tcp")
	if background {
		req.Header.Set("X-Kiri-Background", "1")
	}
	res, err := e.http.Do(req)
	if err != nil {
		return stream, err
	}
	if res.StatusCode != http.StatusSwitchingProtocols {
		defer res.Body.Close()
		return stream, responseError(res)
	}
	rw, ok := res.Body.(io.ReadWriteCloser)
	if !ok {
		res.Body.Close()
		return stream, fmt.Errorf("engine did not provide a duplex exec stream")
	}
	stream.ReadWriteCloser = rw
	stream.reader = rw
	if !tty {
		stream.reader = &dockerReader{reader: rw}
	}
	return stream, nil
}

// Docker's multiplexing headers can cross HTTP reads. Decode without buffering
// a whole frame, keeping output bounded even when a frame is large.
type dockerReader struct {
	reader    io.Reader
	remaining uint32
}

func (r *dockerReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for r.remaining == 0 {
		var h [8]byte
		_, err := io.ReadFull(r.reader, h[:])
		if err != nil {
			return 0, err
		}
		if (h[0] != 1 && h[0] != 2) || h[1] != 0 || h[2] != 0 || h[3] != 0 {
			return 0, fmt.Errorf("invalid Docker output frame")
		}
		r.remaining = binary.BigEndian.Uint32(h[4:])
	}
	if uint32(len(p)) > r.remaining {
		p = p[:r.remaining]
	}
	n, err := r.reader.Read(p)
	r.remaining -= uint32(n)
	if err == io.EOF && r.remaining > 0 {
		err = io.ErrUnexpectedEOF
	}
	return n, err
}
