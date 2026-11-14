//go:build linux || darwin

package localengine

import (
	"context"
	"syscall"

	"github.com/OpenFabrica/FabriBay/engine/internal/guestwire"
)

// FileContainers returns a snapshot of the running containers shown in Finder.
func (e *Engine) FileContainers() map[string]string {
	e.mu.Lock()
	defer e.mu.Unlock()
	containers := make(map[string]string)
	if e.closed || e.session == nil {
		return containers
	}
	for id, c := range e.containers {
		if c.state == "running" && !c.removing {
			containers[c.name] = id
		}
	}
	return containers
}

func (e *Engine) fileSession(id string) (*guestwire.Session, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	c := e.containers[id]
	if c == nil || c.state != "running" || c.removing || e.closed || e.session == nil {
		return nil, syscall.ESTALE
	}
	return e.session, nil
}

func (e *Engine) FileStat(ctx context.Context, id, path string) (guestwire.FileInfo, error) {
	session, err := e.fileSession(id)
	if err != nil {
		return guestwire.FileInfo{}, err
	}
	return session.FileStat(ctx, id, path)
}

func (e *Engine) FileList(ctx context.Context, id, path string) ([]guestwire.FileInfo, error) {
	session, err := e.fileSession(id)
	if err != nil {
		return nil, err
	}
	return session.FileList(ctx, id, path)
}

func (e *Engine) FileRead(ctx context.Context, query guestwire.FileQuery) ([]byte, error) {
	session, err := e.fileSession(query.ID)
	if err != nil {
		return nil, err
	}
	return session.FileRead(ctx, query)
}
