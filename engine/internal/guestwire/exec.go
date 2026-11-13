package guestwire

import (
	"context"
	"fmt"
)

const ExecMessage Kind = 22

// ExecSpec describes an additional process inside an already running container.
type ExecSpec struct {
	ContainerID, ID string
	Argv, Env       []string
	Cwd, User       string
	Flags           uint32
	Caps            uint64
	Process         ProcessOptions
	Options         []string
}

func (s ExecSpec) Frame() (Frame, error) {
	var e encoder
	e.text(s.ContainerID)
	e.text(s.ID)
	e.list(s.Argv)
	e.list(s.Env)
	e.text(s.Cwd)
	e.text(s.User)
	e.u32(s.Flags)
	e.u64(s.Caps)
	options, err := s.Process.appendOptions(s.Options)
	if err != nil {
		return Frame{}, err
	}
	e.list(options)
	return Frame{Kind: ExecMessage, Payload: e.payload}, e.err
}

// DecodeExec reads the Kiri protocol v4 Exec payload.
func DecodeExec(frame Frame) (ExecSpec, error) {
	d := runDecoder{data: frame.Payload}
	var s ExecSpec
	s.ContainerID, s.ID = d.text(), d.text()
	s.Argv, s.Env = d.list(), d.list()
	s.Cwd, s.User = d.text(), d.text()
	s.Flags, s.Caps = uint32(d.number(4)), d.number(8)
	if len(d.data) > 0 {
		s.Options = d.list()
	}
	if d.err == nil && len(d.data) != 0 {
		d.err = fmt.Errorf("trailing bytes in Exec")
	}
	if d.err != nil {
		return ExecSpec{}, d.err
	}
	var err error
	s.Process, s.Options, err = decodeProcessOptions(s.Options)
	return s, err
}

// Exec uses the same startup reply slot as a container Run.
func (s *Session) Exec(ctx context.Context, spec ExecSpec) (uint32, error) {
	frame, err := spec.Frame()
	if err != nil {
		return 0, err
	}
	reply, err := s.request(ctx, frame, StartedMessage, spec.ID, nil, 0)
	if err != nil {
		return 0, err
	}
	event, err := DecodeRunEvent(reply)
	return event.PID, err
}

const (
	ExecOutputCreditMessage Kind = 77
	ExecOutputCreditOption       = "go-exec-output-credit"
	ExecProcessGroupOption       = "go-exec-process-group"
	ExecOutputWindow             = 64
)

// ExecOutputCredit grants one output frame after it is flushed. A zero
// grant tells the guest to discard future output after the client leaves.
func (s *Session) ExecOutputCredit(ctx context.Context, id string, count uint32) error {
	var e encoder
	e.text(id)
	e.u32(count)
	return s.send(ctx, ExecOutputCreditMessage, e)
}

func DecodeExecOutputCredit(frame Frame) (string, uint32, error) {
	d := runDecoder{data: frame.Payload}
	id := d.text()
	count := uint32(d.number(4))
	if d.err == nil && (len(d.data) != 0 || id == "" || count > ExecOutputWindow) {
		d.err = fmt.Errorf("invalid exec output credit")
	}
	return id, count, d.err
}
