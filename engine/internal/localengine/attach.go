//go:build linux || darwin

package localengine

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"time"

	"github.com/libracoder/kiri/engine/internal/guestwire"
)

func (e *Engine) waitHTTP(w http.ResponseWriter, r *http.Request) {
	condition := r.URL.Query().Get("condition")
	if condition != "" && condition != "not-running" && condition != "next-exit" && condition != "removed" {
		writeError(w, fmt.Errorf("%w: unsupported wait condition %q", errInvalid, condition))
		return
	}
	wait, err := e.prepareWait(r.PathValue("id"), condition)
	if err != nil {
		writeError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := http.NewResponseController(w).Flush(); err != nil {
		return
	}
	code, err := wait(r.Context())
	var failure any
	if err != nil {
		failure = map[string]string{"Message": err.Error()}
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"StatusCode": code, "Error": failure})
}

func (e *Engine) attachHTTP(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if !flagSet(q, "stream") {
		writeError(w, fmt.Errorf("%w: attach requires streaming", errInvalid))
		return
	}
	e.mu.Lock()
	c, err := e.find(r.PathValue("id"))
	if err != nil {
		e.mu.Unlock()
		writeError(w, err)
		return
	}
	tty := c.run.Flags&guestwire.RunTTY != 0
	var attempt *runAttempt
	var inputCtx context.Context
	var inputCancel context.CancelFunc
	if flagSet(q, "stdin") {
		if c.run.Flags&guestwire.RunStdin == 0 || (c.state == "exited" && flagSet(q, "logs")) {
			e.mu.Unlock()
			writeError(w, fmt.Errorf("%w: container has no stdin available", errConflict))
			return
		}
		attempt = c.nextAttempt()
		if attempt.inputCancel != nil {
			e.mu.Unlock()
			writeError(w, fmt.Errorf("%w: container stdin already attached", errConflict))
			return
		}
		// net/http cancels the request context when its hijacked reader
		// sees EOF. Here EOF closes stdin and the response must keep streaming.
		inputCtx, inputCancel = context.WithCancel(context.WithoutCancel(r.Context()))
		attempt.inputCancel = inputCancel
	}
	var logs []record
	if flagSet(q, "logs") {
		logs = slices.Clone(c.logs)
	}
	var stream chan record
	var listener *outputListener
	if c.state != "exited" || !flagSet(q, "logs") {
		dir := ""
		if e.state != nil {
			dir = e.state.dir
		}
		listener = c.listenOutput(dir)
		stream = listener.output
	}
	e.mu.Unlock()
	inputStarted := false
	defer func() {
		if inputCancel != nil {
			inputCancel()
		}
		e.mu.Lock()
		delete(c.listeners, listener)
		if attempt != nil && !inputStarted {
			attempt.inputCancel = nil
		}
		e.mu.Unlock()
		if listener != nil {
			listener.cancel()
		}
	}()
	var outputError func() error
	if listener != nil {
		outputError = listener.failure
	}

	e.streamProcess(w, processAttachment{
		id: c.id, query: q, tty: tty, logs: logs, output: stream,
		attempt: attempt, inputCtx: inputCtx, cancelInput: inputCancel,
		onInput:     func() { inputStarted = true },
		outputError: outputError,
	})
}

// listenOutput and closeOutputListeners are called with e.mu held.
func (c *container) listenOutput(dir string) *outputListener {
	listener := newOutputListener(dir)
	if c.listeners == nil {
		c.listeners = make(map[*outputListener]struct{})
	}
	c.listeners[listener] = struct{}{}
	return listener
}

func (c *container) closeOutputListeners() {
	for listener := range c.listeners {
		listener.finish()
		delete(c.listeners, listener)
	}
}

type processAttachment struct {
	id          string
	query       url.Values
	tty         bool
	logs        []record
	output      <-chan record
	attempt     *runAttempt
	inputCtx    context.Context
	cancelInput context.CancelFunc
	onInput     func()
	onInputDone func()
	onOutput    func() error
	outputError func() error
}

func (e *Engine) streamProcess(w http.ResponseWriter, a processAttachment) {
	conn, rw, err := http.NewResponseController(w).Hijack()
	if err != nil {
		writeError(w, err)
		return
	}
	defer conn.Close()
	flush := func(entry record) error {
		if err := conn.SetWriteDeadline(time.Now().Add(15 * time.Second)); err != nil {
			return err
		}
		if err := writeSelected(rw, a.query, entry, a.tty); err != nil {
			return err
		}
		if err := rw.Flush(); err != nil {
			return err
		}
		if a.onOutput != nil {
			return a.onOutput()
		}
		return nil
	}
	if err := conn.SetWriteDeadline(time.Now().Add(15 * time.Second)); err != nil {
		return
	}
	if _, err := rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nContent-Type: application/vnd.docker.raw-stream\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n"); err != nil {
		return
	}
	if err := rw.Flush(); err != nil {
		return
	}
	if a.attempt != nil {
		if a.onInput != nil {
			a.onInput()
		}
		inputDone := make(chan struct{})
		go func() {
			defer close(inputDone)
			e.forwardInput(a.inputCtx, a.id, a.attempt, rw.Reader)
			if a.onInputDone != nil {
				a.onInputDone()
			}
			// The container may exit while the client still has bytes
			// queued. Drain them so closing the socket preserves stdout EOF.
			_, _ = io.Copy(io.Discard, rw.Reader)
			if a.tty {
				e.mu.Lock()
				a.attempt.inputCancel = nil
				e.mu.Unlock()
			}
		}()
		defer func() {
			a.cancelInput()
			if socket, ok := conn.(interface{ CloseWrite() error }); ok {
				_ = socket.CloseWrite()
			}
			const drainTimeout = time.Second
			_ = conn.SetReadDeadline(time.Now().Add(drainTimeout))
			timer := time.NewTimer(drainTimeout)
			defer timer.Stop()
			select {
			case <-inputDone:
			case <-timer.C:
			}
		}()
	}
	for _, entry := range a.logs {
		if err := flush(entry); err != nil {
			return
		}
	}
	if a.output == nil {
		return
	}
	for {
		select {
		case entry, ok := <-a.output:
			if !ok {
				if a.outputError != nil {
					if err := a.outputError(); err != nil {
						data := []byte(err.Error())
						if !a.tty {
							var header [8]byte
							header[0] = 3 // Docker's system-error stream.
							binary.BigEndian.PutUint32(header[4:], uint32(len(data)))
							_, _ = rw.Write(header[:])
						}
						_, _ = rw.Write(data)
						_ = rw.Flush()
					}
				}
				return
			}
			if flush(entry) != nil {
				return
			}
		}
	}
}

// forwardInput waits for the guest pipe, because Docker attaches before
// starting a newly created container. EOF half-closes only the input stream.
func (e *Engine) forwardInput(ctx context.Context, id string, attempt *runAttempt, reader io.Reader) {
	select {
	case <-attempt.started:
	case <-ctx.Done():
		return
	}
	buffer := make([]byte, guestwire.InputChunk)
	for {
		n, err := reader.Read(buffer)
		if n != 0 {
			if writeErr := e.session.WriteInput(ctx, id, buffer[:n]); writeErr != nil {
				return
			}
		}
		if err != nil {
			// A peer that resets an upgraded connection cannot supply more
			// input either. Let the guest drain its pipe and observe EOF.
			_ = e.session.CloseStdin(ctx, id)
			return
		}
	}
}

func flagSet(q url.Values, key string) bool {
	v := q.Get(key)
	return v == "1" || v == "true"
}

func writeSelected(w io.Writer, q url.Values, entry record, tty bool) error {
	if tty {
		if !flagSet(q, "stdout") && !flagSet(q, "stderr") {
			return nil
		}
		_, err := w.Write(entry.data)
		return err
	}
	stream := "stdout"
	if entry.stream == 2 {
		stream = "stderr"
	}
	if !flagSet(q, stream) {
		return nil
	}
	var head [8]byte
	head[0] = entry.stream
	binary.BigEndian.PutUint32(head[4:], uint32(len(entry.data)))
	if _, err := w.Write(head[:]); err != nil {
		return err
	}
	_, err := w.Write(entry.data)
	return err
}
