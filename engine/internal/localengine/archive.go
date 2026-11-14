//go:build linux || darwin

package localengine

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"syscall"
	"time"

	"github.com/OpenFabrica/FabriBay/engine/internal/guestwire"
	"github.com/OpenFabrica/FabriBay/engine/internal/hostagent"
)

func (e *Engine) archiveHTTP(w http.ResponseWriter, r *http.Request) {
	c, err := e.lockOperation(r.PathValue("id"))
	if err != nil {
		writeError(w, err)
		return
	}
	defer c.operation.Unlock()
	q := r.URL.Query()
	path := q.Get("path")
	if strings.ContainsRune(path, 0) {
		writeError(w, fmt.Errorf("%w: invalid container path", errInvalid))
		return
	}
	spec := guestwire.CopySpec{Op: guestwire.CopyOut, Path: "/" + strings.TrimPrefix(path, "/")}
	switch r.Method {
	case http.MethodHead:
		spec.Op = guestwire.CopyStat
	case http.MethodPut:
		spec.Op, spec.Flags = guestwire.CopyIn, guestwire.CopyChunked
		if flagSet(q, "copyUIDGID") {
			spec.Flags |= guestwire.CopyKeepOwners
		}
		if flagSet(q, "noOverwriteDirNonDir") {
			spec.Flags |= guestwire.CopyNoReplace
		}
	}

	var source io.ReadCloser
	if spec.Op == guestwire.CopyIn {
		source, _, err = decodeArchive(archiveUpload{r.Body, http.NewResponseController(w)}, true)
		if err != nil {
			writeError(w, fmt.Errorf("%w: read container archive: %v", errInvalid, err))
			return
		}
		defer source.Close()
	}
	conn, closeArchive, err := e.openContainerArchive(r.Context(), c, spec)
	if err != nil {
		writeError(w, err)
		return
	}
	defer closeArchive()
	if source != nil {
		if err := uploadContainerArchive(conn, source); err != nil {
			writeError(w, err)
			return
		}
		w.WriteHeader(http.StatusOK)
		return
	}
	frame, err := readContainerArchiveReply(conn)
	if err != nil {
		writeError(w, err)
		return
	}

	stat, err := guestwire.DecodeArchiveStat(frame)
	if err != nil {
		writeError(w, err)
		return
	}
	header, err := json.Marshal(map[string]any{
		"name": stat.Name, "size": stat.Size, "mode": archiveFileMode(stat.Mode),
		"mtime": time.Unix(stat.Sec, int64(stat.Nsec)).UTC(), "linkTarget": stat.Link,
	})
	if err != nil {
		writeError(w, err)
		return
	}
	w.Header().Set("X-Docker-Container-Path-Stat", base64.StdEncoding.EncodeToString(header))
	w.Header().Set("Content-Type", "application/x-tar")
	w.WriteHeader(http.StatusOK)
	if spec.Op == guestwire.CopyOut {
		if _, err := io.Copy(w, conn); err != nil && !errors.Is(err, context.Canceled) {
			panic(http.ErrAbortHandler)
		}
	}
}

func (e *Engine) exportHTTP(w http.ResponseWriter, r *http.Request) {
	c, err := e.lockOperation(r.PathValue("id"))
	if err != nil {
		writeError(w, err)
		return
	}
	defer c.operation.Unlock()
	stream, closeArchive, err := e.openCopyStream(r.Context(), c, guestwire.CopyExport)
	if err != nil {
		writeError(w, err)
		return
	}
	defer closeArchive()
	w.Header().Set("Content-Type", "application/x-tar")
	w.WriteHeader(http.StatusOK)
	if _, err := io.Copy(w, stream); err != nil && r.Context().Err() == nil {
		panic(http.ErrAbortHandler)
	}
}

func (e *Engine) openCopyStream(ctx context.Context, c *container, op uint8) (io.Reader, func(), error) {
	conn, closeArchive, err := e.openContainerArchive(ctx, c, guestwire.CopySpec{
		Op: op, Path: "/", Flags: guestwire.CopyChunked,
	})
	if err != nil {
		return nil, nil, err
	}
	frame, err := readContainerArchiveReply(conn)
	if err == nil && (frame.Kind != guestwire.CopyDoneMessage || len(frame.Payload) != 0) {
		err = fmt.Errorf("unexpected guest archive reply %d", frame.Kind)
	}
	if err != nil {
		closeArchive()
		return nil, nil, err
	}
	return &guestwire.CopyChunkReader{Source: conn}, closeArchive, nil
}

func (e *Engine) diffHTTP(w http.ResponseWriter, r *http.Request) {
	c, err := e.lockOperation(r.PathValue("id"))
	if err != nil {
		writeError(w, err)
		return
	}
	defer c.operation.Unlock()
	stream, closeArchive, err := e.openCopyStream(r.Context(), c, guestwire.CopyDiff)
	if err != nil {
		writeError(w, err)
		return
	}
	defer closeArchive()
	var changes []guestwire.FilesystemChange
	err = json.NewDecoder(stream).Decode(&changes)
	if err == nil {
		_, err = io.Copy(io.Discard, stream)
	}
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, changes)
}

// openContainerArchive requires the caller to hold c.operation.
func (e *Engine) openContainerArchive(ctx context.Context, c *container, spec guestwire.CopySpec) (net.Conn, func(), error) {
	if e.guestArchive == nil {
		return nil, nil, fmt.Errorf("container archive service is unavailable")
	}
	e.mu.Lock()
	running, removed := c.state == "running", c.state == "removed"
	spec.ID, spec.Layers = c.id, c.run.Layers
	e.mu.Unlock()
	if removed {
		return nil, nil, fmt.Errorf("%w: container was removed", errMissing)
	}
	if !running {
		if spec.Op == guestwire.CopyIn && c.run.Flags&guestwire.RunReadOnlyRoot != 0 {
			return nil, nil, fmt.Errorf("%w: container root is read-only", errInvalid)
		}
		if err := hostagent.EnsureLayers(ctx, e.session, spec.Layers, c.blobs); err != nil {
			return nil, nil, err
		}
	}
	frame, err := spec.Frame()
	if err != nil {
		return nil, nil, err
	}
	conn, err := e.guestArchive(ctx, guestwire.CopyPort)
	if err != nil {
		return nil, nil, err
	}
	stopRequest := context.AfterFunc(ctx, func() { _ = conn.Close() })
	stopEngine := context.AfterFunc(e.ctx, func() { _ = conn.Close() })
	closeArchive := func() {
		stopRequest()
		stopEngine()
		_ = conn.Close()
	}
	if err := guestwire.WriteFrame(conn, frame); err != nil {
		closeArchive()
		return nil, nil, err
	}
	return conn, closeArchive, nil
}

func uploadContainerArchive(conn io.ReadWriter, input io.Reader) error {
	if err := guestwire.WriteCopyChunks(conn, input); err != nil {
		return fmt.Errorf("%w: write container archive: %v", errInvalid, err)
	}
	frame, err := readContainerArchiveReply(conn)
	if err != nil {
		return err
	}
	if frame.Kind != guestwire.CopyDoneMessage || len(frame.Payload) != 0 {
		return fmt.Errorf("unexpected guest archive reply %d", frame.Kind)
	}
	return nil
}

func readContainerArchiveReply(r io.Reader) (guestwire.Frame, error) {
	frame, err := guestwire.ReadFrame(r, nil)
	if err == nil && frame.Kind == guestwire.ErrorMessage {
		err = containerArchiveError(string(frame.Payload))
	}
	return frame, err
}

func containerArchiveError(message string) error {
	if message, ok := strings.CutPrefix(message, "notfound: "); ok {
		return fmt.Errorf("%w: %s", errMissing, message)
	}
	return fmt.Errorf("%w: %s", errInvalid, strings.TrimPrefix(message, "notdir: "))
}

func archiveFileMode(mode uint32) os.FileMode {
	m := os.FileMode(mode & 0777)
	switch mode & syscall.S_IFMT {
	case syscall.S_IFDIR:
		m |= os.ModeDir
	case syscall.S_IFLNK:
		m |= os.ModeSymlink
	case syscall.S_IFBLK:
		m |= os.ModeDevice
	case syscall.S_IFCHR:
		m |= os.ModeDevice | os.ModeCharDevice
	case syscall.S_IFIFO:
		m |= os.ModeNamedPipe
	case syscall.S_IFSOCK:
		m |= os.ModeSocket
	}
	if mode&04000 != 0 {
		m |= os.ModeSetuid
	}
	if mode&02000 != 0 {
		m |= os.ModeSetgid
	}
	if mode&01000 != 0 {
		m |= os.ModeSticky
	}
	return m
}
