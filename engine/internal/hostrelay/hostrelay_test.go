package hostrelay

import (
	"bytes"
	"io"
	"net"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/OpenFabrica/Fabo/engine/internal/wslbridge"
)

// TestLoopback matches upstream's proxy_addresses_on_loopback.
func TestLoopback(t *testing.T) {
	for in, want := range map[string]uint16{
		"127.0.0.1:7892":                   7892,
		"http://user:p@ss@localhost:8080/": 8080,
		"http://[::1]:3128":                3128,
		"socks5://127.0.0.2:1080":          1080,
		"HTTP://LOCALHOST:1/x?y#z":         1,
	} {
		if got, ok := Loopback(in); !ok || got != want {
			t.Errorf("Loopback(%q) = %d, %v; want %d", in, got, ok, want)
		}
	}
	for _, in := range []string{"http://10.0.0.1:3128", "http://proxy.corp:8080", "http://127.0.0.1", "http://127.0.0.1:0", "", "http://127.0.0.1:x", "http://[::2]:80"} {
		if got, ok := Loopback(in); ok {
			t.Errorf("Loopback(%q) = %d; want none", in, got)
		}
	}
	if got := Targets("127.0.0.1:7892", "", "http://localhost:7892", "proxy:80", "[::1]:1080"); len(got) != 2 || got[0] != 1080 || got[1] != 7892 {
		t.Fatalf("Targets = %v", got)
	}
}

// wslProcess is a fake `wsl.exe -- fabo hostrelay`: ListenSide over two
// pipes, as wsl.exe's stdin and stdout carry it.
type wslProcess struct {
	proc *Process
	// crash ends it as WSL shutting down would.
	crash func()
	// ended is closed when its ListenSide has returned.
	ended chan struct{}
}

// startWSL runs ListenSide in this process behind a Process.
func startWSL(t *testing.T) *wslProcess {
	t.Helper()
	inR, inW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	ended := make(chan struct{})
	go func() {
		_ = ListenSide(inR, outW)
		outW.Close()
		inR.Close()
		close(ended)
	}()
	var once sync.Once
	kill := func() { once.Do(func() { inR.Close(); outW.Close() }) }
	return &wslProcess{
		proc: &Process{In: inW, Out: outR, Kill: kill, Wait: func() error {
			<-ended
			outR.Close()
			return nil
		}},
		crash: kill,
		ended: ended,
	}
}

// fakeStarts counts relays started, keeping each.
type fakeStarts struct {
	mu      sync.Mutex
	t       *testing.T
	started []*wslProcess
	distros []string
}

// start is Client.Start.
func (f *fakeStarts) start(distro string) (*Process, error) {
	w := startWSL(f.t)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.started = append(f.started, w)
	f.distros = append(f.distros, distro)
	return w.proc, nil
}

// count is how many relays have started.
func (f *fakeStarts) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.started)
}

// last is the newest relay.
func (f *fakeStarts) last() *wslProcess {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.started[len(f.started)-1]
}

// upperProxy is a "proxy" on this machine's loopback: it echoes each
// connection, upper-cased, after its half-close.
func upperProxy(t *testing.T) uint16 {
	t.Helper()
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				all, _ := io.ReadAll(c)
				_, _ = c.Write(bytes.ToUpper(all))
			}()
		}
	}()
	return uint16(l.Addr().(*net.TCPAddr).Port)
}

// roundTrip sends body through the relay's port in WSL and reads the reply.
func roundTrip(t *testing.T, local uint16, body []byte) []byte {
	c, err := net.Dial("tcp4", "127.0.0.1:"+strconv.Itoa(int(local)))
	if err != nil {
		t.Error(err)
		return nil
	}
	defer c.Close()
	go func() {
		_, _ = c.Write(body)
		_ = c.(*net.TCPConn).CloseWrite()
	}()
	_ = c.SetReadDeadline(time.Now().Add(30 * time.Second))
	got, err := io.ReadAll(c)
	if err != nil {
		t.Error(err)
	}
	return got
}

// refused reports whether nothing listens on local any more.
func refused(local uint16) bool {
	c, err := net.DialTimeout("tcp4", "127.0.0.1:"+strconv.Itoa(int(local)), time.Second)
	if err != nil {
		return true
	}
	c.Close()
	return false
}

// TestConnectionsReachTheProxyAndBack runs both ends in this process, with
// several connections at once, one larger than many frames and the queue,
// each ending with a half-close.
func TestConnectionsReachTheProxyAndBack(t *testing.T) {
	target := upperProxy(t)
	f := &fakeStarts{t: t}
	c := &Client{Start: f.start}
	defer c.Close()
	ports := c.Ports("fabo", []uint16{target, target})
	local := ports[target]
	if local == 0 || len(ports) != 1 {
		t.Fatalf("Ports = %v", ports)
	}
	if again := c.Ports("fabo", []uint16{target}); again[target] != local || f.count() != 1 {
		t.Fatalf("asking again: %v, %d relays", again, f.count())
	}
	big := make([]byte, 3_000_000)
	for i := range big {
		big[i] = 'a' + byte(i%26)
	}
	var wg sync.WaitGroup
	for i := range 6 {
		body := []byte("hello " + strconv.Itoa(i))
		if i == 0 {
			body = big
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			if got := roundTrip(t, local, body); !bytes.Equal(got, bytes.ToUpper(body)) {
				t.Errorf("connection %d: %d bytes back, want %d", i, len(got), len(body))
			}
		}()
	}
	wg.Wait()
}

// pair runs ListenSide and DialSide against each other, with the Windows
// end dialling only allowed; it returns the LISTENING replies and a way to
// send LISTEN.
func pair(t *testing.T, allowed uint16) (*sync.Map, func(uint16)) {
	w := startWSL(t)
	t.Cleanup(func() { w.proc.In.Close(); <-w.ended })
	var ports sync.Map
	s := newSender(w.proc.In)
	go func() {
		_ = dialSide(w.proc.Out, s, func(p uint16) bool { return p == allowed },
			func(target, local uint16) { ports.Store(target, local) }, DialLoopback)
	}()
	return &ports, func(p uint16) { _ = s.send(KindListen, 0, portData(p)) }
}

// listenFor asks for target and waits for its port in WSL.
func listenFor(t *testing.T, ports *sync.Map, listen func(uint16), target uint16) uint16 {
	t.Helper()
	listen(target)
	for range 500 {
		if l, ok := ports.Load(target); ok {
			return l.(uint16)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no LISTENING for %d", target)
	return 0
}

// TestOnlyTheAskedPortsAreDialled: a LISTEN the Windows end did not mean
// (here sent past its allowed list) leads nowhere.
func TestOnlyTheAskedPortsAreDialled(t *testing.T) {
	other, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	target := uint16(other.Addr().(*net.TCPAddr).Port)
	ports, listen := pair(t, 1)
	local := listenFor(t, ports, listen, target)
	if local == 0 || listenFor(t, ports, listen, target) != local {
		t.Fatal("asking again should keep the port")
	}
	c, err := net.Dial("tcp4", "127.0.0.1:"+strconv.Itoa(int(local)))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	if n, err := c.Read(make([]byte, 1)); n != 0 || err == nil || os.IsTimeout(err) {
		t.Fatalf("read = %d, %v; want closed", n, err)
	}
	_ = other.(*net.TCPListener).SetDeadline(time.Now().Add(100 * time.Millisecond))
	if a, err := other.Accept(); err == nil {
		a.Close()
		t.Fatal("the port was dialled")
	}
}

// TestAProxyThatIsNotRunningClosesTheConnection: nothing on the Windows
// port, so the connection in WSL ends.
func TestAProxyThatIsNotRunningClosesTheConnection(t *testing.T) {
	gone, _ := net.Listen("tcp4", "127.0.0.1:0")
	target := uint16(gone.Addr().(*net.TCPAddr).Port)
	gone.Close()
	f := &fakeStarts{t: t}
	c := &Client{Start: f.start}
	defer c.Close()
	local := c.Ports("fabo", []uint16{target})[target]
	if local == 0 {
		t.Fatal("no port")
	}
	conn, err := net.Dial("tcp4", "127.0.0.1:"+strconv.Itoa(int(local)))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	if n, err := conn.Read(make([]byte, 1)); n != 0 || err == nil || os.IsTimeout(err) {
		t.Fatalf("read = %d, %v; want closed", n, err)
	}
}

// TestRelayLifecycle: no targets stops it and its listener; a removed port
// or another distro starts a new one; one that ends after answering calls
// Ended; one that never answers does not.
func TestRelayLifecycle(t *testing.T) {
	a, b := upperProxy(t), upperProxy(t)
	f := &fakeStarts{t: t}
	var ended atomic.Int32
	c := &Client{Start: f.start, Ended: func() { ended.Add(1) }, Grace: 200 * time.Millisecond}
	defer c.Close()

	first := c.Ports("fabo", []uint16{a})[a]
	if first == 0 {
		t.Fatal("no port")
	}
	// Adding a port keeps the relay.
	both := c.Ports("fabo", []uint16{a, b})
	if both[a] != first || both[b] == 0 || f.count() != 1 {
		t.Fatalf("adding: %v, %d relays", both, f.count())
	}
	// Dropping one restarts it, so the old listeners go.
	only := c.Ports("fabo", []uint16{b})
	// (Unless the new listener happened to get the old port number.)
	if f.count() != 2 || only[b] == 0 || !refused(first) || (only[b] != both[b] && !refused(both[b])) {
		t.Fatalf("dropping: %v, %d relays", only, f.count())
	}
	<-f.started[0].ended
	// Another distro: another relay.
	if moved := c.Ports("other", []uint16{b}); moved[b] == 0 || f.count() != 3 || f.distros[2] != "other" {
		t.Fatalf("distro: %v, %v", moved, f.distros)
	}
	// No proxy: the relay ends and nothing listens.
	last := c.Ports("other", []uint16{b})[b]
	if got := c.Ports("other", nil); len(got) != 0 {
		t.Fatalf("no targets = %v", got)
	}
	<-f.last().ended
	if !refused(last) {
		t.Fatal("listener left behind")
	}
	if ended.Load() != 0 {
		t.Fatal("Ended for a relay Fabo stopped")
	}
	// WSL goes away under a working relay: Ended, then a new one.
	local := c.Ports("fabo", []uint16{a})[a]
	f.last().crash()
	deadline := time.Now().Add(5 * time.Second)
	for ended.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if ended.Load() != 1 || !refused(local) {
		t.Fatalf("crash: ended %d", ended.Load())
	}
	n := f.count()
	if again := c.Ports("fabo", []uint16{a}); again[a] == 0 || f.count() != n+1 {
		t.Fatalf("after crash: %v", again)
	}
	if got := roundTrip(t, c.Ports("fabo", []uint16{a})[a], []byte("ok")); string(got) != "OK" {
		t.Fatalf("after crash: %q", got)
	}
}

// TestOldFaboWithoutHostrelay: a relay that ends at once (an older `fabo`
// in WSL) gives no ports, quickly, and no Ended.
func TestOldFaboWithoutHostrelay(t *testing.T) {
	var ended atomic.Int32
	c := &Client{
		Start: func(string) (*Process, error) {
			r, w := io.Pipe()
			return &Process{In: nopWriteCloser{}, Out: r, Kill: func() {}, Wait: func() error { return nil }}, w.Close()
		},
		Ended: func() { ended.Add(1) },
	}
	start := time.Now()
	if got := c.Ports("fabo", []uint16{7892}); len(got) != 0 {
		t.Fatalf("Ports = %v", got)
	}
	if time.Since(start) > 5*time.Second || ended.Load() != 0 {
		t.Fatalf("took %v, ended %d", time.Since(start), ended.Load())
	}
	c.Close()
}

// TestFramesMatchUpstream pins the bytes of LISTEN and LISTENING.
func TestFramesMatchUpstream(t *testing.T) {
	var buf bytes.Buffer
	s := newSender(&buf)
	_ = s.send(KindListen, 0, portData(7892))
	_ = s.send(KindListening, 0, portData(7892, 41234))
	_ = s.send(KindOpen, 3, portData(7892))
	want := []byte{
		6, 0, 0, 0, 0, 0, 0, 0, 2, 0x1e, 0xd4,
		7, 0, 0, 0, 0, 0, 0, 0, 4, 0x1e, 0xd4, 0xa1, 0x12,
		1, 0, 0, 0, 3, 0, 0, 0, 2, 0x1e, 0xd4,
	}
	if !bytes.Equal(buf.Bytes(), want) {
		t.Fatalf("frames = % x", buf.Bytes())
	}
	f, err := wslbridge.ReadFrame(bytes.NewReader(want[11:]))
	if err != nil || f.Kind != KindListening || len(f.Data) != 4 {
		t.Fatalf("read back = %+v, %v", f, err)
	}
}

// nopWriteCloser discards what is written.
type nopWriteCloser struct{}

func (nopWriteCloser) Write(b []byte) (int, error) { return len(b), nil }
func (nopWriteCloser) Close() error                { return nil }
