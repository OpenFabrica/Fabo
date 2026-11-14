package guestagent

import (
	"testing"
	"testing/synctest"

	"github.com/OpenFabrica/FabriBay/engine/internal/guestwire"
)

// An exec cannot fill the host queue past its credit, and a detached
// client must release the producer without emitting the pending output.
func TestExecOutputBackpressure(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		output := newExecOutput()
		frames := 0
		emit := output.wrap(func(event guestwire.RunEvent) error {
			if event.Kind == guestwire.OutputMessage {
				frames++
			}
			return nil
		})
		done := make(chan struct{})
		go func() {
			defer close(done)
			for range guestwire.ExecOutputWindow + 2 {
				_ = emit(guestwire.RunEvent{Kind: guestwire.OutputMessage})
			}
		}()
		synctest.Wait()
		if frames != guestwire.ExecOutputWindow {
			t.Errorf("sent %d frames with a window of %d", frames, guestwire.ExecOutputWindow)
		}
		output.grant(1)
		synctest.Wait()
		if frames != guestwire.ExecOutputWindow+1 {
			t.Errorf("sent %d frames after a one-frame grant", frames)
		}
		output.grant(0)
		<-done
		if frames != guestwire.ExecOutputWindow+1 {
			t.Errorf("sent %d frames after disconnect", frames)
		}
	})
}
