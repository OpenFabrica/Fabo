package hostrelay

import (
	"fmt"
	"io"
	"maps"
	"net"
	"slices"
	"strings"
	"sync"
	"time"
)

// Process is a running `wsl.exe -- fabo hostrelay`: frames go to In and
// come from Out. Kill ends it at once; Wait reaps it after Out has ended.
type Process struct {
	In   io.WriteCloser
	Out  io.Reader
	Kill func()
	Wait func() error
}

// Client is the Windows end's lifecycle (upstream crates/kiri-win/src/relay.rs):
// one relay process into the engine's distro, started when a proxy on
// Windows' loopback needs it, and stopped when none does.
type Client struct {
	// Start runs `fabo hostrelay` in distro.
	Start func(distro string) (*Process, error)
	// Dial reaches a proxy port on Windows; DialLoopback when nil.
	Dial func(port uint16) (net.Conn, error)
	// Logf, when set, gets one line per change of outcome.
	Logf func(format string, args ...any)
	// Ended, when set, is called after a relay that had answered ends on
	// its own (WSL shut down, the distro restarted): the caller pushes the
	// proxy again, which starts a new one.
	Ended func()
	// Answer is how long Ports waits for the relay; 10 seconds when 0.
	Answer time.Duration
	// Grace is how long Close lets the relay end after its input closes
	// before killing it; 2 seconds when 0.
	Grace time.Duration

	mu      sync.Mutex
	cur     *relay
	logMu   sync.Mutex
	lastLog string
}

// relay is one running relay process.
type relay struct {
	distro string
	proc   *Process
	s      *sender
	done   chan struct{}

	mu sync.Mutex
	// allowed are the Windows ports it was asked to listen for: the only
	// ones dialled.
	allowed map[uint16]bool
	// ports maps a Windows port to the distro's; 0 when it could not listen.
	ports map[uint16]uint16
	// changed is closed and replaced whenever ports or dead change.
	changed        chan struct{}
	dead, stopping bool
	answered       bool
}

// notifyLocked wakes everything waiting on r.changed.
func (r *relay) notifyLocked() {
	close(r.changed)
	r.changed = make(chan struct{})
}

// logOnce logs msg unless it was the last thing logged, so a minute's
// retry does not repeat it.
func (c *Client) logOnce(msg string) {
	c.logMu.Lock()
	defer c.logMu.Unlock()
	if msg == c.lastLog || c.Logf == nil {
		return
	}
	c.lastLog = msg
	c.Logf("%s", msg)
}

// Ports returns, for each Windows loopback port in targets, the distro's
// port that leads to it, starting the relay into distro when it is not
// running. Ports it could not relay are missing: an older `fabo` in WSL
// without hostrelay, or no answer in time. With no targets the relay
// stops, and no listener is left in WSL.
//
// A running relay is replaced when it has ended, when the distro changed,
// or when it listens for a port no longer wanted (the protocol has no way
// to stop one listener, and a stale one would still lead to Windows).
func (c *Client) Ports(distro string, targets []uint16) map[uint16]uint16 {
	targets = slices.Compact(slices.Sorted(slices.Values(targets)))
	targets = slices.DeleteFunc(targets, func(p uint16) bool { return p == 0 })
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(targets) == 0 || distro == "" {
		c.stopLocked()
		return map[uint16]uint16{}
	}
	if r := c.cur; r != nil {
		r.mu.Lock()
		stale := r.dead || r.distro != distro
		for p := range r.allowed {
			stale = stale || !slices.Contains(targets, p)
		}
		r.mu.Unlock()
		if stale {
			c.stopLocked()
		}
	}
	if c.cur == nil {
		r, err := c.start(distro)
		if err != nil {
			c.logOnce(fmt.Sprintf("proxy relay: %v", err))
			return map[uint16]uint16{}
		}
		c.cur = r
	}
	r := c.cur
	var ask []uint16
	r.mu.Lock()
	for _, t := range targets {
		// A port it could not listen for is asked again: the WSL end
		// remembers only the listeners it has.
		if l, ok := r.ports[t]; !r.allowed[t] || (ok && l == 0) {
			r.allowed[t] = true
			delete(r.ports, t)
			ask = append(ask, t)
		}
	}
	r.mu.Unlock()
	for _, t := range ask {
		if r.s.send(KindListen, 0, portData(t)) != nil {
			break
		}
	}
	wait := c.Answer
	if wait == 0 {
		wait = 10 * time.Second
	}
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	out := map[uint16]uint16{}
	for {
		r.mu.Lock()
		all := true
		for _, t := range targets {
			if _, ok := r.ports[t]; !ok {
				all = false
			}
		}
		dead, changed := r.dead, r.changed
		if all || dead {
			for _, t := range targets {
				if l := r.ports[t]; l != 0 {
					out[t] = l
				}
			}
		}
		r.mu.Unlock()
		if all || dead {
			break
		}
		select {
		case <-changed:
			continue
		case <-deadline.C:
		}
		// Out of time: what it has answered so far.
		r.mu.Lock()
		for _, t := range targets {
			if l := r.ports[t]; l != 0 {
				out[t] = l
			}
		}
		r.mu.Unlock()
		break
	}
	if len(out) == len(targets) {
		var parts []string
		for _, t := range slices.Sorted(maps.Keys(out)) {
			parts = append(parts, fmt.Sprintf("Windows 127.0.0.1:%d is 127.0.0.1:%d in WSL", t, out[t]))
		}
		c.logOnce("proxy relay: " + strings.Join(parts, ", "))
	} else {
		c.logOnce(fmt.Sprintf("proxy relay: no answer for %v; those proxies are not used", targets))
	}
	return out
}

// Close stops the relay, if one runs: its listeners in WSL close with it.
func (c *Client) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stopLocked()
}

// stopLocked ends the current relay and waits for it: its input closes,
// which ends `fabo hostrelay`, and after the grace period it is killed.
func (c *Client) stopLocked() {
	r := c.cur
	if r == nil {
		return
	}
	c.cur = nil
	r.mu.Lock()
	r.stopping = true
	r.mu.Unlock()
	_ = r.proc.In.Close()
	grace := c.Grace
	if grace == 0 {
		grace = 2 * time.Second
	}
	select {
	case <-r.done:
	case <-time.After(grace):
		r.proc.Kill()
		<-r.done
	}
}

// start runs a relay into distro and its frame loop.
func (c *Client) start(distro string) (*relay, error) {
	p, err := c.Start(distro)
	if err != nil {
		return nil, err
	}
	r := &relay{
		distro: distro, proc: p, s: newSender(p.In), done: make(chan struct{}),
		allowed: map[uint16]bool{}, ports: map[uint16]uint16{}, changed: make(chan struct{}),
	}
	dial := c.Dial
	if dial == nil {
		dial = DialLoopback
	}
	allowed := func(port uint16) bool {
		r.mu.Lock()
		defer r.mu.Unlock()
		return r.allowed[port]
	}
	listening := func(target, local uint16) {
		r.mu.Lock()
		defer r.mu.Unlock()
		if r.allowed[target] {
			r.ports[target] = local
			r.answered = true
			r.notifyLocked()
		}
	}
	go func() {
		end := dialSide(p.Out, r.s, allowed, listening, dial)
		r.mu.Lock()
		r.dead = true
		r.notifyLocked()
		stopping, answered := r.stopping, r.answered
		r.mu.Unlock()
		// Its input too, so a relay that ended on its own leaves nothing.
		_ = p.In.Close()
		status := p.Wait()
		close(r.done)
		if !stopping {
			c.logOnce(fmt.Sprintf("proxy relay ended: %v, %v", end, status))
			// Only one that worked: an older `fabo` without hostrelay ends
			// at once, and restarting it at once would only spin.
			if answered && c.Ended != nil {
				c.Ended()
			}
		}
	}()
	return r, nil
}
