package guestagent

import (
	"bytes"
	"fmt"
	"os"
	"sync"

	"github.com/OpenFabrica/Fabo/engine/internal/guestwire"
)

// childInput keeps pipe backpressure off the shared control reader. The
// queued count includes data already handed to a blocked pipe write.
type childInput struct {
	pipe *os.File
	wake chan struct{}
	done chan struct{}

	mu      sync.Mutex
	buffer  bytes.Buffer
	queued  int
	closing bool
}

func newChildInput(pipe *os.File, ack func(uint32) error) *childInput {
	input := &childInput{pipe: pipe, wake: make(chan struct{}, 1), done: make(chan struct{})}
	go input.drain(ack)
	return input
}

func (i *childInput) notify() {
	select {
	case i.wake <- struct{}{}:
	default:
	}
}

func (i *childInput) enqueue(data []byte) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.closing {
		return nil
	}
	if len(data) > guestwire.InputWindow-i.queued {
		return fmt.Errorf("container stdin window exceeded")
	}
	i.queued += len(data)
	i.buffer.Write(data)
	i.notify()
	return nil
}

func (i *childInput) closeStdin() {
	i.mu.Lock()
	i.closing = true
	i.mu.Unlock()
	i.notify()
}

func (i *childInput) abort() {
	i.mu.Lock()
	i.buffer.Reset()
	i.closing = true
	i.mu.Unlock()
	_ = i.pipe.Close()
	i.notify()
	<-i.done
}

func (i *childInput) drain(ack func(uint32) error) {
	defer close(i.done)
	defer i.pipe.Close()
	buf := make([]byte, guestwire.InputChunk)
	discard := false
	for {
		i.mu.Lock()
		size, _ := i.buffer.Read(buf)
		closing := i.closing
		i.mu.Unlock()
		if size == 0 {
			if closing {
				return
			}
			<-i.wake
			continue
		}
		if !discard {
			_, err := i.pipe.Write(buf[:size])
			// A process may close fd 0 and keep running. Discard further
			// input while returning credit to the attached client.
			discard = err != nil
		}
		i.mu.Lock()
		i.queued -= size
		i.mu.Unlock()
		if ack(uint32(size)) != nil {
			return
		}
	}
}

func (r *Runtime) input(id string) *childInput {
	r.mu.Lock()
	defer r.mu.Unlock()
	if child := r.children[id]; child != nil {
		return child.input
	}
	return nil
}

// The PTY master has no independent write half. Keep it open after detach.
func (r *Runtime) closeStdin(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if child := r.children[id]; child != nil && child.terminal == nil && child.input != nil {
		child.input.closeStdin()
	}
}
