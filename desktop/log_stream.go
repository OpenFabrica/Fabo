package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

const maxLiveLogBytes = 1 << 20
const maxLiveLogLines = 5000

// logSnapshot is a copy of a stream's lines; first is the number of the
// first one, counted from the start of the stream or its last clearing.
type logSnapshot struct {
	lines            []string
	first            int
	problem          string
	finished, loaded bool
}
type liveLogs struct {
	key              string
	cancel           context.CancelFunc
	done             chan struct{}
	mu               sync.Mutex
	lines            []string
	partial          string
	bytes            int
	problem          string
	loaded, finished bool
	revision         uint64
	// dropped counts the lines let go to bound the buffer, so that the
	// ones kept keep their numbers.
	dropped int
}

func (l *liveLogs) close() { l.cancel() }

// clear forgets the lines read so far; the stream goes on, and what comes
// next is numbered from one.
func (l *liveLogs) clear() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines, l.partial, l.bytes, l.dropped = nil, "", 0, 0
	l.revision++
}
func (l *liveLogs) snapshot() logSnapshot {
	l.mu.Lock()
	defer l.mu.Unlock()
	lines := append([]string(nil), l.lines...)
	if l.partial != "" {
		lines = append(lines, l.partial)
	}
	return logSnapshot{lines, l.dropped + 1, l.problem, l.finished, l.loaded}
}
func (l *liveLogs) append(data []byte) {
	l.mu.Lock()
	defer l.mu.Unlock()
	text := l.partial + string(data)
	parts := strings.Split(text, "\n")
	l.partial = parts[len(parts)-1]
	if len(l.partial) > maxLiveLogBytes {
		l.partial = l.partial[len(l.partial)-maxLiveLogBytes:]
	}
	for _, line := range parts[:len(parts)-1] {
		line = strings.TrimSuffix(line, "\r")
		l.lines = append(l.lines, line)
		l.bytes += len(line) + 1
	}
	for len(l.lines) > 0 && (len(l.lines) > maxLiveLogLines || l.bytes+len(l.partial) > maxLiveLogBytes) {
		l.bytes -= len(l.lines[0]) + 1
		l.lines = l.lines[1:]
		l.dropped++
	}
	l.loaded = true
	l.revision++
}

// A reader owns its buffer and connection. The main thread takes snapshots;
// redraws are coalesced at 60 ms, including output without a final newline.
func newLiveLogs(parent context.Context, key string, notify func(), open func(context.Context) (io.ReadCloser, func() error, error)) *liveLogs {
	ctx, cancel := context.WithCancel(parent)
	l := &liveLogs{key: key, cancel: cancel, done: make(chan struct{})}
	go func() {
		tick := time.NewTicker(60 * time.Millisecond)
		defer tick.Stop()
		var seen uint64
		for {
			select {
			case <-l.done:
				notify()
				return
			case <-tick.C:
				l.mu.Lock()
				revision := l.revision
				l.mu.Unlock()
				if revision != seen {
					seen = revision
					notify()
				}
			}
		}
	}()
	go func() {
		defer close(l.done)
		reader, finish, err := open(ctx)
		if finish != nil {
			defer func() {
				if cleanupErr := finish(); cleanupErr != nil {
					l.mu.Lock()
					l.problem = cleanupErr.Error()
					fmt.Fprintln(os.Stderr, "Fabo: log stream ended:", cleanupErr)
					l.mu.Unlock()
				}
			}()
		}
		if err == nil {
			// Cancellation releases a blocked read, even when the source is silent.
			stop := context.AfterFunc(ctx, func() { _ = reader.Close() })
			defer stop()
			defer reader.Close()
			data := make([]byte, 16<<10)
			for {
				n, readErr := reader.Read(data)
				if n > 0 {
					l.append(data[:n])
				}
				if readErr != nil {
					err = readErr
					break
				}
			}
		}
		l.mu.Lock()
		l.finished = true
		l.loaded = true
		if err != nil && err != io.EOF && ctx.Err() == nil {
			l.problem = err.Error()
		}
		l.mu.Unlock()
	}()
	return l
}
func (e engineClient) openContainerLogs(ctx context.Context, id string) (io.ReadCloser, func() error, error) {
	read, cancel := context.WithTimeout(ctx, 10*time.Second)
	var details struct{ Config struct{ Tty bool } }
	err := e.get(read, containerPath(id)+"/json", &details)
	cancel()
	if err != nil {
		return nil, nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://localhost"+containerPath(id)+"/logs?stdout=1&stderr=1&follow=1&tail=5000", nil)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("X-Kiri-Background", "1")
	res, err := e.http.Do(req)
	if err != nil {
		return nil, nil, err
	}
	if res.StatusCode != http.StatusOK {
		defer res.Body.Close()
		return nil, nil, responseError(res)
	}
	var reader io.Reader = res.Body
	if !details.Config.Tty {
		reader = &dockerReader{reader: res.Body}
	}
	return struct {
		io.Reader
		io.Closer
	}{reader, res.Body}, nil, nil
}
func (e engineClient) openPodLogs(ctx context.Context, ns, pod, container string, previous bool) (io.ReadCloser, func() error, error) {
	if !validKubeName(ns) || !validKubeName(pod) || !validKubeName(container) {
		return nil, nil, fmt.Errorf("invalid Kubernetes log target")
	}
	args := []string{"kubectl", "logs", "-n", ns, pod, "-c", container, "--tail=2000", "--timestamps=true"}
	if previous {
		args = append(args, "--previous", "--request-timeout=15s")
	} else {
		args = append(args, "--follow", "--request-timeout=0")
	}
	stream, err := e.openExec(ctx, kubernetesNode, args, false, false, true)
	if stream == nil {
		return nil, nil, err
	}
	finish := func() error {
		var exitErr error
		if ctx.Err() == nil {
			check, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			var state struct {
				Running  bool
				ExitCode int
			}
			if err := e.get(check, "/exec/"+url.PathEscape(stream.id)+"/json", &state); err != nil {
				exitErr = err
			} else if state.Running {
				exitErr = fmt.Errorf("log stream ended before kubectl exited")
			} else if state.ExitCode != 0 {
				exitErr = fmt.Errorf("kubectl logs exited with code %d", state.ExitCode)
			}
			cancel()
		}
		_ = stream.Close()
		<-stream.done
		if stream.cleanupErr != nil {
			return stream.cleanupErr
		}
		return exitErr
	}
	return stream, finish, err
}

func (s *desktop) syncLogStreams() {
	containerKey, podKey := "", ""
	if s.connected && s.page == containersPage && s.tab[containersPage] == 1 {
		if x, ok := s.container(s.selected[containersPage]); ok {
			containerKey = x.ID + "/" + fmt.Sprint(x.Started)
		}
	}
	if s.page == kubernetesPage && s.tab[kubernetesPage] == 1 {
		for _, o := range s.kube.items {
			if o.key() == s.selected[kubernetesPage] && o.Kind == "Pod" && s.kube.logsContainer != "" {
				podKey = podLogKey(o, s.kube.logsContainer, s.kube.logsPrevious)
				if s.podLogs == nil || s.podLogs.key != podKey {
					if s.podLogs != nil {
						s.retireLogs(s.podLogs)
					}
					ns, name, container, previous := o.Metadata.Namespace, o.Metadata.Name, s.kube.logsContainer, s.kube.logsPrevious
					s.podLogs = newLiveLogs(s.ctx, podKey, s.invalidate, func(ctx context.Context) (io.ReadCloser, func() error, error) {
						return s.engine.openPodLogs(ctx, ns, name, container, previous)
					})
				}
				break
			}
		}
	}
	if containerKey == "" {
		if s.containerLogs != nil {
			s.retireLogs(s.containerLogs)
			s.containerLogs = nil
		}
	} else if s.containerLogs == nil || s.containerLogs.key != containerKey {
		if s.containerLogs != nil {
			s.retireLogs(s.containerLogs)
		}
		id := s.selected[containersPage]
		s.containerLogs = newLiveLogs(s.ctx, containerKey, s.invalidate, func(ctx context.Context) (io.ReadCloser, func() error, error) {
			return s.engine.openContainerLogs(ctx, id)
		})
	}
	if podKey == "" && s.podLogs != nil {
		s.retireLogs(s.podLogs)
		s.podLogs = nil
	}
}

func (s *desktop) visibleContainerLogs() logSnapshot {
	if x, ok := s.container(s.selected[containersPage]); ok && s.containerLogs != nil && s.containerLogs.key == x.ID+"/"+fmt.Sprint(x.Started) {
		return s.containerLogs.snapshot()
	}
	return logSnapshot{}
}
func podLogKey(o kubeObject, container string, previous bool) string {
	return o.key() + "/" + o.Metadata.UID + "/" + container + "/" + fmt.Sprint(previous)
}
