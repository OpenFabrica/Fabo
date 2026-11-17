package main

import (
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/OpenFabrica/Fabo/engine/internal/domaindns"
)

// `<name>.fabo.local` on Windows, as /etc/resolver/fabo.local is on the
// Mac (upstream crates/kiri-win/src/dns.rs): a name-resolution policy
// (NRPT) rule sends `.fabo.local` lookups to fabo-win's DNS answerer on
// 127.0.0.1:53, which answers 127.0.0.1; WSL forwards the engine's domain
// router from its loopback to Windows', and the router sorts requests out
// by name. NRPT has no port, so the answerer needs 53 itself, unlike the
// Mac's 53535. Adding or removing the rule is the only step that needs an
// administrator; Windows asks each time.
//
// Whether names are on is read from the machine every time (the rule in
// the registry and the answerer holding the port), never kept in a file,
// so a crash or a change made outside Fabo needs no reconciling.

// errCanceled is a declined administrator prompt: a choice, not a failure.
// The window matches this exact word and stays quiet.
var errCanceled = errors.New("canceled")

// errPort53 is turning names on while another program holds 127.0.0.1:53:
// the rule would send lookups to it, not to Fabo.
var errPort53 = errors.New("another program holds 127.0.0.1:53, so Fabo cannot answer for <name>.fabo.local; containers stay at <name>.fabo.localhost")

// errNamesBusy is a switch asked for while another one waits; 409 on the
// control pipe, as errBusy is.
var errNamesBusy = errors.New("container names are already being changed")

// nrptNamespace is the namespace of Fabo's NRPT rule.
const nrptNamespace = "." + domaindns.Suffix

// dnsSystem is what the answerer and its rule do to Windows, so the logic
// is tested without it.
type dnsSystem interface {
	// listenDNS binds 127.0.0.1:53 for UDP, exclusively.
	listenDNS() (net.PacketConn, error)
	// nrptRuleInstalled reports whether Windows has Fabo's NRPT rule.
	nrptRuleInstalled() bool
	// installNRPTRule adds (on) or removes the rule as administrator;
	// errCanceled when the prompt is declined.
	installNRPTRule(on bool) error
	logf(format string, args ...any)
}

// domainsView is GET /domains on the control pipe, and what `fabo domains`
// prints for the window (the macOS launcher's domainState, plus why).
type domainsView struct {
	// On: *.fabo.local resolves here, which needs both halves below.
	On     bool   `json:"on"`
	Suffix string `json:"suffix"`
	// HTTPS with Fabo's certificate is the Mac's only.
	HTTPS     bool `json:"https"`
	Installed bool `json:"installed"`
	Serving   bool `json:"serving"`
}

// localNames runs the *.fabo.local answerer and switches its NRPT rule.
type localNames struct {
	sys  dnsSystem
	mu   sync.Mutex // guards conn
	conn net.PacketConn
	// busy is a switch in progress (an administrator prompt can wait for
	// minutes); a second one is refused rather than queued.
	busy atomic.Bool
}

// newLocalNames returns the answerer, not yet listening.
func newLocalNames(sys dnsSystem) *localNames { return &localNames{sys: sys} }

// start binds 127.0.0.1:53 and answers on it, unless it already does.
// Windows' Internet Connection Sharing holds 0.0.0.0:53, which the
// loopback address still wins over.
func (n *localNames) start() error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.conn != nil {
		return nil
	}
	conn, err := n.sys.listenDNS()
	if err != nil {
		return err
	}
	n.conn = conn
	go n.serve(conn)
	return nil
}

// serving reports whether the answerer holds the port.
func (n *localNames) serving() bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.conn != nil
}

// close stops answering (tests; fabo-win's port goes with the process).
func (n *localNames) close() {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.conn != nil {
		n.conn.Close()
		n.conn = nil
	}
}

// serve answers queries on conn until it is closed. Any local process can
// send to the port: a read error other than the close (Windows reports a
// client's closed port as WSAECONNRESET on the next read) is skipped, and
// a panic on one packet must not take fabo-win and docker's pipe down.
func (n *localNames) serve(conn net.PacketConn) {
	buf := make([]byte, 4096)
	for {
		size, from, err := conn.ReadFrom(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				n.mu.Lock()
				if n.conn == conn {
					n.conn = nil
				}
				n.mu.Unlock()
				return
			}
			time.Sleep(10 * time.Millisecond)
			continue
		}
		func() {
			defer func() {
				if r := recover(); r != nil {
					n.sys.logf("dns: %v", r)
				}
			}()
			if reply := domaindns.Answer(buf[:size]); reply != nil {
				_, _ = conn.WriteTo(reply, from)
			}
		}()
	}
}

// view reads the state from the machine.
func (n *localNames) view() domainsView {
	v := domainsView{Installed: n.sys.nrptRuleInstalled(), Serving: n.serving(), Suffix: "fabo.localhost"}
	// The rule alone is not enough: nothing answers if port 53 was taken.
	if v.On = v.Installed && v.Serving; v.On {
		v.Suffix = domaindns.Suffix
	}
	return v
}

// set turns *.fabo.local on or off. On needs the port first: with it held
// by another program the rule would send lookups there, so no prompt is
// shown. Off is always allowed, so a rule left behind while the port was
// lost can still be removed. The machine is read again afterwards: an
// exit code alone does not prove the rule is (or is not) there.
func (n *localNames) set(on bool) error {
	if !n.busy.CompareAndSwap(false, true) {
		return errNamesBusy
	}
	defer n.busy.Store(false)
	if on {
		if err := n.start(); err != nil {
			n.sys.logf("dns on 127.0.0.1:53: %v", err)
			return errPort53
		}
	}
	if n.sys.nrptRuleInstalled() == on {
		return nil
	}
	if err := n.sys.installNRPTRule(on); err != nil {
		return err
	}
	if n.sys.nrptRuleInstalled() != on {
		return fmt.Errorf("the DNS rule for %s could not be %s", nrptNamespace, map[bool]string{true: "added", false: "removed"}[on])
	}
	n.sys.logf("dns: *.fabo.local %s", map[bool]string{true: "on", false: "off"}[on])
	return nil
}

// nrptHasNamespace reports whether a rule's Name value (REG_MULTI_SZ, one
// namespace per string) covers Fabo's namespace.
func nrptHasNamespace(names []string) bool {
	for _, name := range names {
		if strings.EqualFold(strings.TrimSpace(name), nrptNamespace) {
			return true
		}
	}
	return false
}
