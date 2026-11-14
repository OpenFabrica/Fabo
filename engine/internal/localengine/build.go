//go:build linux || darwin

package localengine

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/OpenFabrica/FabriBay/engine/internal/guestwire"
	"github.com/google/go-containerregistry/pkg/authn"
)

const maxBuildDockerfile = 8 << 20

func (e *Engine) buildHTTP(w http.ResponseWriter, r *http.Request) {
	options, err := parseBuildOptions(r.URL.Query())
	if err != nil {
		writeError(w, fmt.Errorf("%w: %v", errInvalid, err))
		return
	}
	if err := e.checkBuilderRuntime(); err != nil {
		writeError(w, err)
		return
	}
	credentials, err := buildRegistryConfig(r.Header.Get("X-Registry-Config"))
	if err != nil {
		writeError(w, err)
		return
	}
	ctx, finish, err := e.beginBuild(r.Context(), options.buildID)
	if err != nil {
		writeError(w, err)
		return
	}
	defer finish()

	ctrl := http.NewResponseController(w)
	if err := ctrl.EnableFullDuplex(); err != nil {
		writeError(w, err)
		return
	}
	abortRead := context.AfterFunc(ctx, func() { _ = ctrl.SetReadDeadline(time.Now()) })
	defer abortRead()

	var input io.Reader
	if options.remote == "" {
		decoded, _, err := decodeArchive(archiveUpload{r.Body, ctrl}, true)
		if err != nil {
			writeError(w, fmt.Errorf("%w: read build context: %v", errInvalid, err))
			return
		}
		defer decoded.Close()
		input, err = prepareBuildContext(decoded, &options)
		if err != nil {
			writeError(w, fmt.Errorf("%w: read build context: %v", errInvalid, err))
			return
		}
	}
	w.Header().Set("Content-Type", "application/json")
	progress := &buildProgress{out: json.NewEncoder(w), ctrl: ctrl, quiet: options.quiet}
	if err := progress.stream("Preparing Fabo's BuildKit builder\n"); err != nil {
		return
	}

	id, tags, err := e.classicBuild(ctx, options, input, credentials, progress)
	if err != nil {
		message := err.Error()
		_ = progress.line(map[string]any{"error": message, "errorDetail": map[string]string{"message": message}})
		return
	}
	if err := progress.line(map[string]any{"aux": map[string]string{"ID": id}}); err != nil {
		return
	}
	if options.quiet {
		_ = progress.line(map[string]string{"stream": id + "\n"})
		return
	}
	if err := progress.stream("Successfully built " + strings.TrimPrefix(id, "sha256:")[:12] + "\n"); err != nil {
		return
	}
	for _, tag := range tags {
		if err := progress.stream("Successfully tagged " + shortImageReference(tag) + "\n"); err != nil {
			return
		}
	}
}

func (e *Engine) beginBuild(parent context.Context, id string) (context.Context, func(), error) {
	ctx, cancel := context.WithCancel(parent)
	stop := context.AfterFunc(e.ctx, cancel)
	if id != "" {
		e.mu.Lock()
		if _, exists := e.builds[id]; exists {
			e.mu.Unlock()
			stop()
			cancel()
			return nil, nil, fmt.Errorf("%w: build %q is already active", errConflict, id)
		}
		if e.builds == nil {
			e.builds = make(map[string]context.CancelFunc)
		}
		e.builds[id] = cancel
		e.mu.Unlock()
	}
	return ctx, func() {
		stop()
		cancel()
		if id != "" {
			e.mu.Lock()
			delete(e.builds, id)
			e.mu.Unlock()
		}
	}, nil
}

func (e *Engine) cancelBuildHTTP(w http.ResponseWriter, r *http.Request) {
	e.mu.Lock()
	cancel := e.builds[r.URL.Query().Get("id")]
	e.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	w.WriteHeader(http.StatusOK)
}

type buildProgress struct {
	out   *json.Encoder
	ctrl  *http.ResponseController
	quiet bool
	tail  []byte
}

func (p *buildProgress) line(value any) error {
	_ = p.ctrl.SetWriteDeadline(time.Now().Add(15 * time.Second))
	defer p.ctrl.SetWriteDeadline(time.Time{})
	if err := p.out.Encode(value); err != nil {
		return err
	}
	return p.ctrl.Flush()
}

func (p *buildProgress) stream(s string) error {
	if p.quiet {
		return nil
	}
	return p.line(map[string]string{"stream": s})
}

func (p *buildProgress) Write(b []byte) (int, error) {
	const limit = 8 << 10
	if len(b) >= limit {
		p.tail = append(p.tail[:0], b[len(b)-limit:]...)
	} else {
		keep := min(len(p.tail), limit-len(b))
		copy(p.tail[:keep], p.tail[len(p.tail)-keep:])
		p.tail = append(p.tail[:keep], b...)
	}
	return len(b), p.stream(string(b))
}

func prepareBuildContext(decoded io.Reader, options *buildOptions) (io.Reader, error) {
	stream := bufio.NewReader(decoded)
	head, err := stream.Peek(512)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	if len(head) == 0 || len(head) >= 262 && bytes.Equal(head[257:262], []byte("ustar")) {
		// Compose can send an archive whose last entry isn't padded.
		return io.MultiReader(stream, bytes.NewReader(make([]byte, 1024))), nil
	}
	text, err := io.ReadAll(io.LimitReader(stream, maxBuildDockerfile+1))
	if err != nil {
		return nil, err
	}
	if len(text) > maxBuildDockerfile {
		return nil, fmt.Errorf("Dockerfile exceeds %d bytes", maxBuildDockerfile)
	}
	options.dockerfile = "Dockerfile"
	return buildFileArchive(options.dockerfile, 0644, text)
}

func buildFileArchive(name string, mode int64, data []byte) (io.Reader, error) {
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: mode, Size: int64(len(data))}); err != nil {
		return nil, err
	}
	if _, err := tw.Write(data); err != nil {
		return nil, err
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return &b, nil
}

func buildRegistryConfig(encoded string) ([]byte, error) {
	if encoded == "" {
		return nil, nil
	}
	unpadded := strings.TrimRight(encoded, "=")
	raw, err := base64.RawURLEncoding.DecodeString(unpadded)
	if err != nil {
		raw, err = base64.RawStdEncoding.DecodeString(unpadded)
	}
	var auth map[string]authn.AuthConfig
	if err != nil || json.Unmarshal(raw, &auth) != nil {
		return nil, fmt.Errorf("%w: invalid build registry authentication header", errInvalid)
	}
	if len(auth) == 0 {
		return nil, nil
	}
	return json.Marshal(map[string]any{"auths": auth})
}

func (e *Engine) classicBuild(ctx context.Context, o buildOptions, input io.Reader, credentials []byte, progress *buildProgress) (string, []string, error) {
	release := e.holdBuilder()
	defer release()
	c, err := e.readyBuilder(ctx, true)
	if err != nil {
		return "", nil, err
	}
	nonce := rand.Text()
	dir := "/tmp/fabo-build-" + nonce
	authDir := "/dev/shm/fabo-build-" + nonce
	// Cleanup is independent of the request. Authentication lives in
	// tmpfs so a builder stop also discards credentials.
	defer func() {
		cleanup, cancel := context.WithTimeout(e.ctx, 5*time.Second)
		defer cancel()
		_ = e.runBuilderCommand(cleanup, c, nil, []string{"rm", "-rf", dir, authDir}, io.Discard, io.Discard)
	}()
	if err := e.runBuilderCommand(ctx, c, nil, []string{"mkdir", "-p", dir + "/context"}, io.Discard, progress); err != nil {
		return "", nil, err
	}
	if input != nil {
		if err := e.uploadBuildArchive(ctx, c, dir+"/context", input); err != nil {
			return "", nil, fmt.Errorf("upload build context: %w", err)
		}
	}
	var env []string
	if len(credentials) != 0 {
		if err := e.runBuilderCommand(ctx, c, nil, []string{"mkdir", "-m", "700", authDir}, io.Discard, progress); err != nil {
			return "", nil, err
		}
		archive, err := buildFileArchive("config.json", 0600, credentials)
		if err != nil {
			return "", nil, err
		}
		if err := e.uploadBuildArchive(ctx, c, authDir, archive); err != nil {
			return "", nil, fmt.Errorf("upload build registry configuration: %w", err)
		}
		env = []string{"DOCKER_CONFIG=" + authDir}
	}

	stage, err := os.MkdirTemp(e.state.dir, ".build-image-")
	if err != nil {
		return "", nil, err
	}
	defer os.RemoveAll(stage)
	type stagedResult struct {
		archive stagedArchive
		err     error
	}
	staged := make(chan stagedResult, 1)
	inputPipe, outputPipe := io.Pipe()
	go func() {
		archive, err := readImageArchive(ctx, stage, inputPipe)
		_ = inputPipe.CloseWithError(err)
		staged <- stagedResult{archive, err}
	}()
	err = e.runBuilderCommand(ctx, c, env, o.command(dir), outputPipe, progress)
	_ = outputPipe.CloseWithError(err)
	result := <-staged
	if err != nil {
		if ctx.Err() != nil {
			return "", nil, ctx.Err()
		}
		if last := strings.TrimSpace(string(progress.tail)); last != "" {
			return "", nil, fmt.Errorf("%w: %s", err, last)
		}
		return "", nil, err
	}
	if result.err != nil {
		return "", nil, result.err
	}
	plan, err := planImageImport(ctx, result.archive)
	if err != nil {
		return "", nil, err
	}
	tags := slices.Sorted(maps.Keys(plan.tags))
	var id string
	if len(tags) != 0 {
		id = plan.tags[tags[0]].ID
	} else {
		for image := range plan.untagged {
			id = image
			break
		}
	}
	if id == "" {
		return "", nil, fmt.Errorf("BuildKit returned no image")
	}
	if err := e.importImages(ctx, plan, "load"); err != nil {
		return "", nil, err
	}
	for _, tag := range tags {
		e.emitImage(plan.tags[tag], "tag", tag)
	}
	return id, tags, nil
}

func (e *Engine) uploadBuildArchive(ctx context.Context, c *container, dest string, input io.Reader) error {
	c.operation.Lock()
	defer c.operation.Unlock()
	conn, closeArchive, err := e.openContainerArchive(ctx, c, guestwire.CopySpec{
		Op: guestwire.CopyIn, Path: dest, Flags: guestwire.CopyChunked | guestwire.CopyNoReplace,
	})
	if err != nil {
		return err
	}
	defer closeArchive()
	return uploadContainerArchive(conn, input)
}
