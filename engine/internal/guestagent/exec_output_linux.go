package guestagent

import (
	"sync"

	"github.com/OpenFabrica/FabriBay/engine/internal/guestwire"
)

// Backpressure stays in this exec's output pumps. The control loop can
// continue to handle input, other processes, and shutdown.
type execOutput struct {
	tokens chan struct{}
	done   chan struct{}
	once   sync.Once
}

func newExecOutput() *execOutput {
	o := &execOutput{tokens: make(chan struct{}, guestwire.ExecOutputWindow), done: make(chan struct{})}
	o.grant(guestwire.ExecOutputWindow)
	return o
}

func (o *execOutput) grant(count uint32) {
	if count == 0 {
		o.once.Do(func() { close(o.done) })
		return
	}
	for range count {
		select {
		case o.tokens <- struct{}{}:
		default:
			return
		}
	}
}

func (o *execOutput) wrap(emit func(guestwire.RunEvent) error) func(guestwire.RunEvent) error {
	return func(event guestwire.RunEvent) error {
		if event.Kind == guestwire.OutputMessage {
			select {
			case <-o.tokens:
			case <-o.done:
				return nil
			}
			select {
			case <-o.done:
				return nil
			default:
			}
		}
		return emit(event)
	}
}

func (r *Runtime) execOutputCredit(id string, count uint32) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if child := r.children[id]; child != nil && child.execOutput != nil {
		child.execOutput.grant(count)
	}
}
