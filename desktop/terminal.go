package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/egoist/mygo/plugins/terminal"
	"github.com/egoist/mygo/ui"
)

const terminalPage = "terminal"

// A session outlives navigation. Only its worker and Close touch resources;
// snapshots let the UI observe startup/exit without racing the reader.
type terminalSession struct {
	label, container string
	started          int64
	cancel           context.CancelFunc
	done             chan struct{}
	mu               sync.Mutex
	term             *terminal.Terminal
	closed, exited   bool
	problem          string
	code             int
}

func (t *terminalSession) snapshot() (*terminal.Terminal, bool, string, int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.term, t.exited, t.problem, t.code
}
func (t *terminalSession) close() {
	t.cancel()
	t.mu.Lock()
	t.closed = true
	term := t.term
	t.mu.Unlock()
	if term != nil {
		_ = term.Close()
	}
}
func newTerminalSession(parent context.Context, label, container string, started int64, notify func(), open func(context.Context) (*terminal.Terminal, *execStream, error)) *terminalSession {
	ctx, cancel := context.WithCancel(parent)
	s := &terminalSession{label: label, container: container, started: started, cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(s.done)
		term, stream, err := open(ctx)
		if stream != nil {
			defer func() {
				_ = stream.Close()
				<-stream.done
				s.mu.Lock()
				if stream.cleanupErr != nil {
					s.problem = stream.cleanupErr.Error()
					fmt.Fprintln(os.Stderr, "Fabo: closing terminal exec:", stream.cleanupErr)
				}
				s.mu.Unlock()
				notify()
			}()
		}
		if err != nil {
			s.mu.Lock()
			if ctx.Err() == nil {
				s.problem = err.Error()
			}
			s.mu.Unlock()
			notify()
			return
		}
		s.mu.Lock()
		s.term = term
		closed := s.closed || ctx.Err() != nil
		s.mu.Unlock()
		if closed {
			_ = term.Close()
			return
		}
		notify()
		select {
		case <-ctx.Done():
			_ = term.Close()
			<-term.Done()
		case <-term.Done():
			code := term.ExitCode()
			if stream != nil {
				check, stop := context.WithTimeout(context.Background(), 5*time.Second)
				var state struct{ ExitCode *int }
				statusErr := stream.engine.get(check, "/exec/"+stream.id+"/json", &state)
				stop()
				if statusErr == nil && state.ExitCode != nil {
					code = *state.ExitCode
				} else {
					s.mu.Lock()
					s.problem = "Could not read terminal exit status"
					s.mu.Unlock()
				}
			}
			s.mu.Lock()
			s.exited = true
			s.code = code
			s.mu.Unlock()
		}
		notify()
	}()
	return s
}
func (s *desktop) invalidate() {
	if s.window != nil {
		s.window.Invalidate()
	}
}

// consoleTheme is the container terminal's own dark look, the same in
// light and dark windows, as a console reads best on a dark ground.
var consoleTheme = func() *terminal.Theme {
	t := terminal.DarkTheme()
	t.Background, t.Foreground, t.Cursor = ui.Hex("#0B0F14"), ui.Hex("#E6EDF3"), ui.Hex("#E6EDF3")
	t.Selection = ui.Hex("#264F78")
	return t
}()

// containerShell starts the container's best interactive shell with a
// tinted user@host:dir prompt. bash and BusyBox read the prompt from PS1;
// bash skips its rc files, which would set their own. Other shells, such
// as dash, cannot expand that prompt and keep their plain one.
const containerShell = `p='\[\033[38;5;117m\]\u@\h:\w\$\[\033[0m\] '
if command -v bash >/dev/null 2>&1; then export PS1="$p"; exec bash --norc -i; fi
case "$(readlink -f /bin/sh 2>/dev/null)" in *busybox*) export PS1="$p";; esac
exec /bin/sh -i`

func (s *desktop) openContainerTerminal(x container) {
	key := "container/" + x.ID
	if s.terminals[key] != nil {
		return
	}
	font := terminal.Font{Family: s.prefs.Mono, Size: s.prefs.LogSize}
	engine := s.engine
	s.terminals[key] = newTerminalSession(s.ctx, x.Name, x.ID, x.Started, s.invalidate, func(ctx context.Context) (*terminal.Terminal, *execStream, error) {
		if err := terminal.Load(); err != nil {
			return nil, nil, err
		}
		stream, err := engine.openExec(ctx, x.ID, []string{"/bin/sh", "-c", containerShell}, true, true, false)
		if err != nil {
			return nil, stream, err
		}
		term, err := terminal.New(terminal.Options{Conn: stream, Font: font, Scrollback: 4 << 20,
			Theme: consoleTheme, DarkTheme: consoleTheme, Padding: terminal.Padding{X: 14, Y: 12}})
		return term, stream, err
	})
}

// dropExitedTerminal forgets a container's shell that has ended, so that
// opening the Terminal tab again starts a new one.
func (s *desktop) dropExitedTerminal(x container) {
	key := "container/" + x.ID
	if t := s.terminals[key]; t != nil {
		if _, exited, _, _ := t.snapshot(); exited {
			s.retireTerminal(key)
		}
	}
}

// interactiveTerminalTab is the container's shell: a line saying where it
// runs, then the terminal in a dark, rounded well. The session lives on
// when the tab or page changes; an ended one is replaced the next time the
// tab opens.
func (s *desktop) interactiveTerminalTab(c *ui.Context, p palette, x container) {
	if x.State != "running" {
		if empty(c, p, "terminal", tr("容器未运行", "The container is not running"), tr("启动", "Start")) {
			s.startContainer(x)
		}
		return
	}
	key := "container/" + x.ID
	if s.terminals[key] == nil {
		s.openContainerTerminal(x)
	}
	session := s.terminals[key]
	term, exited, problem, code := session.snapshot()
	hint, ink := fmt.Sprintf(tr("在 %s 中打开 shell；离开此页后会话仍保留。", "Opens a shell in %s; the session stays open when you leave this tab."), x.Name), p.sub
	switch {
	case problem != "":
		hint, ink = problem, p.err
	case exited:
		hint = fmt.Sprintf(tr("shell 已退出（代码 %d）；重新打开此页签可再次连接。", "The shell exited (code %d); open this tab again to reconnect."), code)
	case term == nil:
		hint = tr("正在连接终端…", "Connecting terminal…")
	}
	ui.Text(c, hint).FontSize(12.5).LineHeight(1.45).TextColor(ink).Padding(12, 16, 10)
	well := ui.Column(c.Key("terminal-well")).Grow(1).MinHeight(0).Margin(0, 16, 16).Radius(10).Clip().Background(consoleTheme.Background)
	well.Children(func() {
		if term != nil {
			terminal.View(c.Key("container-terminal"), term).Fill().AutoFocus().Label(tr("容器交互终端", "Container terminal"))
		}
	})
}

// The shell picker only offers installed shells, matching upstream's allowlist.
func installedShells() []string {
	var candidates []string
	if runtime.GOOS == "windows" {
		for _, name := range []string{"pwsh.exe", "powershell.exe", "cmd.exe"} {
			if path, err := exec.LookPath(name); err == nil {
				candidates = append(candidates, path)
			}
		}
		return candidates
	}
	candidates = append(candidates, os.Getenv("SHELL"))
	if raw, err := os.ReadFile("/etc/shells"); err == nil {
		for _, line := range strings.Split(string(raw), "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "/") {
				candidates = append(candidates, line)
			}
		}
	}
	for _, dir := range []string{"/opt/homebrew/bin", "/usr/local/bin", "/home/linuxbrew/.linuxbrew/bin"} {
		for _, name := range []string{"zsh", "bash", "fish", "nu", "pwsh"} {
			candidates = append(candidates, filepath.Join(dir, name))
		}
	}
	var result []string
	seen := map[string]bool{}
	for _, path := range candidates {
		name := filepath.Base(path)
		if path == "" || name == "nologin" || name == "false" || name == "true" || name == "rbash" || name == "git-shell" || name == "screen" || name == "tmux" {
			continue
		}
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
			continue
		}
		real, err := filepath.EvalSymlinks(path)
		if err != nil || seen[real] {
			continue
		}
		seen[real] = true
		result = append(result, path)
	}
	return result
}
func terminalEnvironment(prefsFile string) []string {
	var dirs []string
	if prefsFile != "" {
		dirs = append(dirs, filepath.Join(filepath.Dir(prefsFile), "bin"))
	}
	if exe, err := os.Executable(); err == nil {
		dirs = append(dirs, filepath.Join(filepath.Dir(exe), "../Resources/bin"), filepath.Dir(exe))
	}
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		dirs = append(dirs, dir)
	}
	return []string{"PATH=" + strings.Join(dirs, string(os.PathListSeparator)), "TERM_PROGRAM=Fabo"}
}
func (s *desktop) openHostTerminal() {
	shells := installedShells()
	allowed := false
	for _, path := range shells {
		if path == s.hostShell {
			allowed = true
		}
	}
	if !allowed {
		if len(shells) == 0 {
			s.notices = append(s.notices, tr("没有可用的 shell", "No available shell"))
			return
		}
		s.hostShell = shells[0]
	}
	s.hostSequence++
	key := "host/" + strconv.Itoa(s.hostSequence)
	shell := s.hostShell
	home, err := os.UserHomeDir()
	if err != nil {
		s.notices = append(s.notices, err.Error())
		return
	}
	opts := terminal.Options{Command: []string{shell}, Dir: home, Env: terminalEnvironment(s.prefsFile), Font: terminal.Font{Family: s.prefs.Mono, Size: s.prefs.LogSize}, Scrollback: 4 << 20}
	if runtime.GOOS == "darwin" {
		opts.Command = append(opts.Command, "-l")
	}
	s.terminals[key] = newTerminalSession(s.ctx, filepath.Base(shell)+" "+strconv.Itoa(s.hostSequence), "", 0, s.invalidate, func(ctx context.Context) (*terminal.Terminal, *execStream, error) {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		term, err := terminal.New(opts)
		return term, nil, err
	})
	s.hostSessions = append(s.hostSessions, key)
	s.hostActive = key
}
func (s *desktop) hostTerminalView(c *ui.Context, p palette) {
	shells := installedShells()
	if s.hostShell == "" && len(shells) > 0 {
		s.hostShell = shells[0]
	}
	toolbar(c, func() {
		if shell, ok := pick(c.Key("shell"), tr("Shell", "Shell"), s.hostShell, shells); ok {
			s.hostShell = shell
		}
		var labels []string
		current := ""
		for _, key := range s.hostSessions {
			if t := s.terminals[key]; t != nil {
				labels = append(labels, t.label)
				if key == s.hostActive {
					current = t.label
				}
			}
		}
		if len(labels) > 0 {
			if v, ok := pick(c.Key("session"), tr("终端会话", "Terminal session"), current, labels); ok {
				for _, key := range s.hostSessions {
					if t := s.terminals[key]; t != nil && t.label == v {
						s.hostActive = key
					}
				}
			}
		}
		ui.Box(c).Grow(1)
		if button(c.Key("new-host-terminal"), p, tr("新建终端", "New terminal"), "plus", primaryButton, len(shells) == 0) {
			s.openHostTerminal()
		}
		if t := s.terminals[s.hostActive]; t != nil {
			if button(c.Key("close-host-terminal"), p, tr("关闭会话", "Close session"), "x", plainButton, false) {
				s.retireTerminal(s.hostActive)
				for i, key := range s.hostSessions {
					if key == s.hostActive {
						s.hostSessions = append(s.hostSessions[:i], s.hostSessions[i+1:]...)
						break
					}
				}
				s.hostActive = ""
				if len(s.hostSessions) > 0 {
					s.hostActive = s.hostSessions[len(s.hostSessions)-1]
				}
			}
		}
	})
	session := s.terminals[s.hostActive]
	if session == nil {
		empty(c, p, "terminal", tr("宿主终端", "Host terminal"), "")
		return
	}
	term, exited, problem, code := session.snapshot()
	if problem != "" {
		remark(c, p, problem)
	}
	if term == nil {
		if problem == "" {
			note(c, p, tr("正在启动 shell…", "Starting shell…"))
		}
		return
	}
	if exited {
		remark(c, p, fmt.Sprintf(tr("会话已结束，退出码 %d", "Session ended with code %d"), code))
	}
	terminal.View(c.Key(s.hostActive), term).Fill().AutoFocus().Label(tr("宿主交互终端", "Host terminal session"))
}

// Stop sessions for removed/restarted containers. Host shells and a selected
// container's shell keep their state across page and dock navigation.
func (s *desktop) reconcileTerminals() {
	for key, t := range s.terminals {
		if t.container != "" {
			x, ok := s.container(t.container)
			if !ok || (x.State != "running" && x.State != "paused") || x.Started != t.started {
				s.retireTerminal(key)
			}
		}
	}
}
func (s *desktop) closeInteractive() {
	var done []<-chan struct{}
	for _, t := range s.closingTerminals {
		done = append(done, t.done)
	}
	for _, l := range s.closingLogs {
		done = append(done, l.done)
	}
	for _, t := range s.terminals {
		t.close()
		done = append(done, t.done)
	}
	for _, logs := range []*liveLogs{s.containerLogs, s.podLogs} {
		if logs != nil {
			logs.close()
			done = append(done, logs.done)
		}
	}
	timeout := time.NewTimer(7 * time.Second)
	defer timeout.Stop()
	for _, d := range done {
		select {
		case <-d:
		case <-timeout.C:
			fmt.Fprintln(os.Stderr, "Fabo: timed out closing desktop sessions")
			return
		}
	}
}

func (s *desktop) retireTerminal(key string) {
	if t := s.terminals[key]; t != nil {
		t.close()
		s.closingTerminals = append(s.closingTerminals, t)
		delete(s.terminals, key)
	}
}
func (s *desktop) retireLogs(logs *liveLogs) {
	if logs != nil {
		logs.close()
		s.closingLogs = append(s.closingLogs, logs)
	}
}
func (s *desktop) reapClosedSessions() {
	terms := s.closingTerminals[:0]
	for _, t := range s.closingTerminals {
		select {
		case <-t.done:
		default:
			terms = append(terms, t)
		}
	}
	s.closingTerminals = terms
	logs := s.closingLogs[:0]
	for _, l := range s.closingLogs {
		select {
		case <-l.done:
		default:
			logs = append(logs, l)
		}
	}
	s.closingLogs = logs
}
