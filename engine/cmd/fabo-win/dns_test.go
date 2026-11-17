package main

import (
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

// fakeDNS is Windows for the answerer: a loopback port, a rule in memory
// and an administrator who approves, declines or whose script fails.
type fakeDNS struct {
	mu       sync.Mutex
	portErr  error
	rule     bool
	decline  bool
	noEffect bool // the script "succeeds" without changing the rule
	prompts  int
	block    chan struct{}
}

// listenDNS binds a free loopback port, or fails as told.
func (f *fakeDNS) listenDNS() (net.PacketConn, error) {
	f.mu.Lock()
	err := f.portErr
	f.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return net.ListenPacket("udp4", "127.0.0.1:0")
}

// nrptRuleInstalled reads the rule in memory.
func (f *fakeDNS) nrptRuleInstalled() bool { f.mu.Lock(); defer f.mu.Unlock(); return f.rule }

// installNRPTRule counts a prompt and acts as the fake administrator does.
func (f *fakeDNS) installNRPTRule(on bool) error {
	if f.block != nil {
		<-f.block
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.prompts++
	if f.decline {
		return errCanceled
	}
	if !f.noEffect {
		f.rule = on
	}
	return nil
}

// logf drops log lines.
func (f *fakeDNS) logf(string, ...any) {}

// TestLocalNamesAnswersOnItsPort checks the answerer replies to a real
// query and keeps serving after garbage.
func TestLocalNamesAnswersOnItsPort(t *testing.T) {
	n := newLocalNames(&fakeDNS{})
	if err := n.start(); err != nil {
		t.Fatal(err)
	}
	defer n.close()
	addr := n.conn.LocalAddr()
	c, err := net.Dial("udp4", addr.String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_, _ = c.Write([]byte{0xff, 0x00, 0x01})
	query := dnsmessage.Message{
		Header:    dnsmessage.Header{ID: 9},
		Questions: []dnsmessage.Question{{Name: dnsmessage.MustNewName("web.shop.fabo.local."), Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET}},
	}
	packet, _ := query.Pack()
	if _, err := c.Write(packet); err != nil {
		t.Fatal(err)
	}
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 512)
	size, err := c.Read(buf)
	if err != nil {
		t.Fatal(err)
	}
	var reply dnsmessage.Message
	if err := reply.Unpack(buf[:size]); err != nil || reply.ID != 9 || len(reply.Answers) != 1 {
		t.Fatalf("reply %+v, %v", reply, err)
	}
	if a := reply.Answers[0].Body.(*dnsmessage.AResource); a.A != [4]byte{127, 0, 0, 1} {
		t.Fatalf("answer %v", a.A)
	}
}

// TestLocalNamesSwitch walks the switch through approval, refusal, a held
// port, a script that did nothing, and a second switch while one waits.
func TestLocalNamesSwitch(t *testing.T) {
	sys := &fakeDNS{}
	n := newLocalNames(sys)
	defer n.close()
	if v := n.view(); v.On || v.Suffix != "fabo.localhost" || v.Serving {
		t.Fatalf("fresh view %+v", v)
	}
	// On: the port first, then the rule.
	if err := n.set(true); err != nil {
		t.Fatal(err)
	}
	if v := n.view(); !v.On || v.Suffix != "fabo.local" || !v.Installed || !v.Serving || v.HTTPS {
		t.Fatalf("on view %+v", v)
	}
	// Already on: no prompt.
	if err := n.set(true); err != nil || sys.prompts != 1 {
		t.Fatalf("again: %v, %d prompts", err, sys.prompts)
	}
	// A declined prompt is errCanceled, and nothing changed.
	sys.decline = true
	if err := n.set(false); !errors.Is(err, errCanceled) || err.Error() != "canceled" || !n.view().On {
		t.Fatalf("declined: %v %+v", err, n.view())
	}
	sys.decline = false
	// A script that exits 0 without a change is still an error.
	sys.noEffect = true
	if err := n.set(false); err == nil || errors.Is(err, errCanceled) {
		t.Fatalf("no effect: %v", err)
	}
	sys.noEffect = false
	if err := n.set(false); err != nil || n.view().On || n.view().Installed {
		t.Fatalf("off: %v %+v", err, n.view())
	}
	// Already off: no prompt.
	before := sys.prompts
	if err := n.set(false); err != nil || sys.prompts != before {
		t.Fatalf("off again: %v", err)
	}
}

// TestLocalNamesPortHeld checks that without the port on goes nowhere near
// the rule, while a rule left behind can still be removed.
func TestLocalNamesPortHeld(t *testing.T) {
	sys := &fakeDNS{portErr: errors.New("address in use"), rule: true}
	n := newLocalNames(sys)
	if err := n.start(); err == nil {
		t.Fatal("started on a held port")
	}
	if v := n.view(); v.On || !v.Installed || v.Serving {
		t.Fatalf("held view %+v", v)
	}
	if err := n.set(true); !errors.Is(err, errPort53) || sys.prompts != 0 {
		t.Fatalf("on with the port held: %v, %d prompts", err, sys.prompts)
	}
	if err := n.set(false); err != nil || sys.rule {
		t.Fatalf("off with the port held: %v", err)
	}
	// The port freed later: on binds it then.
	sys.mu.Lock()
	sys.portErr = nil
	sys.mu.Unlock()
	if err := n.set(true); err != nil || !n.view().On {
		t.Fatalf("on once free: %v %+v", err, n.view())
	}
	n.close()
	if n.view().On {
		t.Fatal("on without the answerer")
	}
}

// TestLocalNamesBusy refuses a second switch while a prompt waits, and
// still reads the state meanwhile.
func TestLocalNamesBusy(t *testing.T) {
	sys := &fakeDNS{block: make(chan struct{})}
	n := newLocalNames(sys)
	defer n.close()
	done := make(chan error)
	go func() { done <- n.set(true) }()
	deadline := time.Now().Add(5 * time.Second)
	for !n.busy.Load() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	// Wait until it is past the port and waiting on the prompt.
	for !n.serving() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if err := n.set(false); !errors.Is(err, errNamesBusy) {
		t.Fatalf("second switch: %v", err)
	}
	_ = n.view()
	close(sys.block)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// TestNRPTNamespace matches the rule's Name values.
func TestNRPTNamespace(t *testing.T) {
	if !nrptHasNamespace([]string{".corp.example", ".Fabo.Local"}) {
		t.Fatal("missed .Fabo.Local")
	}
	if nrptHasNamespace([]string{"fabo.local", ".fabo.local.example", ".notfabo.local"}) || nrptHasNamespace(nil) {
		t.Fatal("matched another namespace")
	}
}
