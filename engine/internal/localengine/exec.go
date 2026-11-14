//go:build linux || darwin

package localengine

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/OpenFabrica/Fabo/engine/internal/guestwire"
)

var errHealthProbeBusy = errors.New("container operation in progress")

type execProcess struct {
	spec           guestwire.ExecSpec
	container      *container
	containerRun   *runAttempt
	attempt        *runAttempt
	running        bool
	pid            uint32
	exit           *int32
	stream         chan record
	healthProbe    <-chan struct{}
	internal       bool
	stdout, stderr bool
	size           [2]uint16
}

func (x *execProcess) private() bool { return x.internal || x.healthProbe != nil }

func newExecID() (string, error) {
	var random [32]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(random[:]), nil
}

func (e *Engine) createExecHTTP(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Cmd, Env                                                 []string
		WorkingDir, User                                         string
		Tty, AttachStdin, AttachStdout, AttachStderr, Privileged bool
		ConsoleSize                                              [2]uint16
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
		writeError(w, fmt.Errorf("%w: %v", errInvalid, err))
		return
	}
	if len(body.Cmd) == 0 || body.Cmd[0] == "" {
		writeError(w, fmt.Errorf("%w: exec requires a command", errInvalid))
		return
	}
	id, err := newExecID()
	if err != nil {
		writeError(w, err)
		return
	}
	e.mu.Lock()
	c, err := e.find(r.PathValue("id"))
	if err == nil && c.paused && isBuildKit(c) {
		e.mu.Unlock()
		c.operation.Lock()
		defer c.operation.Unlock()
		e.mu.Lock()
		if c.paused {
			e.mu.Unlock()
			err = e.setPaused(r.Context(), c, false)
			e.mu.Lock()
		}
	}
	if err == nil && c.paused {
		err = fmt.Errorf("%w: container is paused, unpause it before exec", errConflict)
	}
	if err == nil && (c.state != "running" || c.removing || e.closed) {
		err = fmt.Errorf("%w: exec requires a running container", errConflict)
	}
	if err != nil {
		e.mu.Unlock()
		writeError(w, err)
		return
	}
	spec := guestwire.ExecSpec{ContainerID: c.id, ID: id, Argv: body.Cmd, Env: mergeEnv(c.runtimeEnv(), body.Env), Cwd: body.WorkingDir, User: body.User}
	spec.Process = c.run.Process
	spec.Caps = c.run.Caps
	spec.Flags = c.run.Flags & guestwire.RunSecurityFlags
	if body.Privileged || c.run.Flags&guestwire.RunPrivileged != 0 {
		spec.Flags |= guestwire.RunPrivileged
	}
	if spec.Cwd == "" {
		spec.Cwd = c.run.Cwd
	}
	if spec.User == "" {
		spec.User = c.run.User
	}
	if body.Tty {
		spec.Flags |= guestwire.RunTTY
		if !slices.ContainsFunc(spec.Env, func(env string) bool { return strings.HasPrefix(env, "TERM=") }) {
			spec.Env = append(spec.Env, "TERM=xterm")
		}
	}
	if body.AttachStdin {
		spec.Flags |= guestwire.RunStdin
	}
	if e.execs == nil {
		e.execs = make(map[string]*execProcess)
	}
	e.execs[id] = &execProcess{spec: spec, container: c, containerRun: c.attempt, stdout: body.AttachStdout, stderr: body.AttachStderr, size: body.ConsoleSize}
	if isBuildKit(c) {
		c.builderUsedAt = time.Now()
	}
	e.emitContainer(c, "exec_create: "+strings.Join(spec.Argv, " "), map[string]string{"execID": id})
	e.mu.Unlock()
	writeJSON(w, http.StatusCreated, map[string]string{"Id": id})
}

func (e *Engine) startExecHTTP(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Detach      bool
		ConsoleSize [2]uint16
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
		writeError(w, fmt.Errorf("%w: %v", errInvalid, err))
		return
	}
	x, attachment, err := e.startExec(r.Context(), r.PathValue("id"), body.Detach, body.ConsoleSize, false)
	if err != nil {
		writeError(w, err)
		return
	}
	if body.Detach {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if attachment.cancelInput != nil {
		defer attachment.cancelInput()
	}
	defer func() {
		e.mu.Lock()
		x.stream = nil
		running := x.running
		e.mu.Unlock()
		if running {
			_ = e.session.ExecOutputCredit(e.ctx, x.spec.ID, 0)
		}
	}()
	e.streamProcess(w, attachment)
}

func (e *Engine) startExec(ctx context.Context, id string, detach bool, size [2]uint16, internal bool) (*execProcess, processAttachment, error) {
	// An exec right after a start waits for the container's setup before
	// taking the container's operation lock, so a CONT, kill or removal can
	// still reach a container stalled while setting up. The checks below
	// reject the exec if that run has ended meanwhile.
	e.mu.Lock()
	var run *runAttempt
	if x := e.execs[id]; x != nil {
		run = x.containerRun
	}
	e.mu.Unlock()
	if err := run.waitSettled(ctx); err != nil {
		return nil, processAttachment{}, err
	}
	e.mu.Lock()
	x := e.execs[id]
	if x == nil || x.private() != internal {
		e.mu.Unlock()
		return nil, processAttachment{}, fmt.Errorf("%w: no such exec instance: %s", errMissing, id)
	}
	c := x.container
	if x.healthProbe != nil {
		if !c.operation.TryLock() {
			e.mu.Unlock()
			return nil, processAttachment{}, errHealthProbeBusy
		}
		c.probeStarting = make(chan struct{})
		defer func() {
			e.mu.Lock()
			c.operation.Unlock()
			close(c.probeStarting)
			c.probeStarting = nil
			e.mu.Unlock()
		}()
	} else {
		e.mu.Unlock()
		c.operation.Lock()
		defer c.operation.Unlock()
		e.mu.Lock()
	}
	if e.closed || x.attempt != nil || c.state != "running" || c.attempt != x.containerRun || e.execs[id] != x {
		e.mu.Unlock()
		return nil, processAttachment{}, fmt.Errorf("%w: exec already started or its container run has ended", errConflict)
	}
	if c.paused {
		e.mu.Unlock()
		if !isBuildKit(c) {
			return nil, processAttachment{}, fmt.Errorf("%w: container is paused, unpause it before exec", errConflict)
		}
		if err := e.setPaused(ctx, c, false); err != nil {
			return nil, processAttachment{}, err
		}
		e.mu.Lock()
		if e.closed || c.state != "running" || c.attempt != x.containerRun || e.execs[id] != x {
			e.mu.Unlock()
			return nil, processAttachment{}, fmt.Errorf("%w: exec container run has ended", errConflict)
		}
	}
	if x.healthProbe != nil && (x.healthProbe != c.attempt.healthChanged || ctx.Err() != nil) {
		e.mu.Unlock()
		return nil, processAttachment{}, fmt.Errorf("%w: health monitor stopped", errConflict)
	}
	x.running = true
	x.attempt = &runAttempt{started: make(chan struct{}), done: make(chan struct{})}
	spec := x.spec
	attachment := processAttachment{id: id, tty: spec.Flags&guestwire.RunTTY != 0}
	if !detach {
		spec.Options = []string{guestwire.ExecOutputCreditOption}
		if x.private() {
			spec.Options = append(spec.Options, guestwire.ExecProcessGroupOption)
		}
		x.stream = make(chan record, guestwire.ExecOutputWindow)
		attachment.onOutput = func() error { return e.session.ExecOutputCredit(e.ctx, id, 1) }
		attachment.output = x.stream
		attachment.query = url.Values{}
		if x.stdout {
			attachment.query.Set("stdout", "1")
		}
		if x.stderr {
			attachment.query.Set("stderr", "1")
		}
		if x.spec.Flags&guestwire.RunStdin != 0 {
			attachment.inputCtx, attachment.cancelInput = context.WithCancel(context.WithoutCancel(ctx))
			attachment.attempt = x.attempt
			x.attempt.inputCancel = attachment.cancelInput
			if !attachment.tty && isBuildKit(c) && len(spec.Argv) == 2 && path.Base(spec.Argv[0]) == "buildctl" && spec.Argv[1] == "dial-stdio" {
				// This exec relays one bidirectional Buildx connection.
				// Once the client disconnects the relay may still wait for
				// BuildKit's socket to close, holding the builder awake.
				attachment.onInputDone = func() { e.stopBuilderCommand(x) }
			}
		}
	}
	if size[0] != 0 && size[1] != 0 {
		x.size = size
	}
	size = x.size
	e.mu.Unlock()

	pid, err := e.session.Exec(ctx, spec)
	if err == nil && x.spec.Flags&guestwire.RunTTY != 0 && size[0] != 0 && size[1] != 0 {
		err = e.session.Resize(e.ctx, id, size[0], size[1])
	}
	e.mu.Lock()
	if err != nil {
		if x.running {
			e.finishExec(x, 255)
		}
	} else if x.running {
		x.pid = pid
	}
	e.mu.Unlock()
	if err != nil {
		return nil, processAttachment{}, err
	}
	return x, attachment, nil
}

func (e *Engine) inspectExecHTTP(w http.ResponseWriter, r *http.Request) {
	e.mu.Lock()
	x := e.execs[r.PathValue("id")]
	if x == nil || x.private() {
		e.mu.Unlock()
		writeError(w, fmt.Errorf("%w: no such exec instance", errMissing))
		return
	}
	body := map[string]any{
		"ID": x.spec.ID, "ContainerID": x.spec.ContainerID, "Running": x.running, "ExitCode": x.exit, "Pid": x.pid,
		"OpenStdin": x.spec.Flags&guestwire.RunStdin != 0, "OpenStdout": x.stdout, "OpenStderr": x.stderr,
		"ProcessConfig": map[string]any{
			"entrypoint": x.spec.Argv[0], "arguments": x.spec.Argv[1:], "tty": x.spec.Flags&guestwire.RunTTY != 0,
			"user": x.spec.User, "privileged": x.spec.Flags&guestwire.RunPrivileged != 0,
		},
	}
	e.mu.Unlock()
	writeJSON(w, http.StatusOK, body)
}

func (e *Engine) resizeExecHTTP(w http.ResponseWriter, r *http.Request) {
	size := terminalSize(r)
	if size[0] == 0 || size[1] == 0 {
		w.WriteHeader(http.StatusOK)
		return
	}
	e.mu.Lock()
	x := e.execs[r.PathValue("id")]
	if x == nil || x.private() {
		e.mu.Unlock()
		writeError(w, fmt.Errorf("%w: no such exec instance", errMissing))
		return
	}
	e.mu.Unlock()
	// An exec has one start. Serialize resize with it so a resize sent
	// before the guest creates its terminal is not lost or overwritten.
	x.container.operation.Lock()
	defer x.container.operation.Unlock()
	e.mu.Lock()
	if e.execs[x.spec.ID] != x {
		e.mu.Unlock()
		writeError(w, fmt.Errorf("%w: no such exec instance", errMissing))
		return
	}
	x.size = size
	running := x.running && x.spec.Flags&guestwire.RunTTY != 0
	e.mu.Unlock()
	if running {
		if err := e.session.Resize(r.Context(), x.spec.ID, size[0], size[1]); err != nil {
			writeError(w, err)
			return
		}
	}
	w.WriteHeader(http.StatusOK)
}

// Called under e.mu by the control session reader.
func (e *Engine) onExecEvent(x *execProcess, event guestwire.RunEvent) {
	if !x.running {
		return
	}
	switch event.Kind {
	case guestwire.StartedMessage:
		x.pid = event.PID
		if !x.private() {
			e.emitContainer(x.container, "exec_start: "+strings.Join(x.spec.Argv, " "), map[string]string{"execID": x.spec.ID})
		}
		close(x.attempt.started)
	case guestwire.OutputMessage:
		if x.stream != nil && len(event.Data) != 0 {
			select {
			case x.stream <- record{stream: event.Stream, data: slices.Clone(event.Data)}:
			default:
				close(x.stream)
				x.stream = nil
			}
		}
	case guestwire.ExitedMessage:
		e.finishExec(x, event.Exit.Code)
	}
}

func (e *Engine) finishExec(x *execProcess, code int32) {
	x.running, x.exit, x.pid = false, &code, 0
	if isBuildKit(x.container) && x.container.attempt == x.containerRun {
		x.container.builderUsedAt = time.Now()
	}
	if !x.private() {
		e.emitContainer(x.container, "exec_die", map[string]string{"execID": x.spec.ID, "exitCode": strconv.FormatInt(int64(code), 10)})
	}
	x.attempt.close()
	if x.stream != nil {
		close(x.stream)
		x.stream = nil
	}
	if x.container.state == "removed" {
		delete(e.execs, x.spec.ID)
	}
}

// stopExecHTTP terminates a desktop-owned exec without stopping its container.
// Closing a Docker TTY stream intentionally detaches; it cannot release a shell
// or a following kubectl process. Keep Docker's detach semantics unchanged.
func (e *Engine) stopExecHTTP(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	e.mu.Lock()
	x := e.execs[id]
	if x == nil || x.private() {
		e.mu.Unlock()
		writeError(w, fmt.Errorf("%w: no such exec instance", errMissing))
		return
	}
	c := x.container
	e.mu.Unlock()
	// Serialize with start so a cancelled open cannot start after its stop.
	c.operation.Lock()
	defer c.operation.Unlock()
	e.mu.Lock()
	if e.execs[id] != x {
		e.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
		return
	}
	running, attempt := x.running, x.attempt
	if attempt == nil {
		delete(e.execs, id)
	}
	e.mu.Unlock()
	if !running {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	if err := e.session.Signal(ctx, id, sigKill); err != nil {
		writeError(w, err)
		return
	}
	select {
	case <-attempt.done:
		w.WriteHeader(http.StatusNoContent)
	case <-ctx.Done():
		writeError(w, ctx.Err())
	}
}
