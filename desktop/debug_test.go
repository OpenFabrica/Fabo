package main

import (
	"fmt"
	"image/png"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"
)

// debugDesktop is a window on the fake engine with its sessions file in a
// temporary directory.
func debugDesktop(t *testing.T) *desktop {
	t.Helper()
	_, s := newFakeEngine(t)
	s.prefs.Lang = "zh"
	s.prefsFile = filepath.Join(t.TempDir(), "desktop.json")
	s.navigate(debugPage)
	t.Cleanup(func() {
		for _, r := range s.debug.runs {
			r.stop()
		}
	})
	return s
}

// settle waits until cond holds, drawing frames meanwhile.
func settle(t *testing.T, tt *ui.Tester, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
		tt.Frame()
	}
	tt.Frame()
}

func TestDebugPageCreatesAndPersistsSessions(t *testing.T) {
	s := debugDesktop(t)
	tt := ui.NewTester(s.view, 1180, 760)
	tt.Frame()
	if !tt.HasText("还没有调试会话") {
		t.Fatal("the empty page is not shown")
	}
	// A TCP server, then a client of it, both through the dialog.
	port := freePort(t, "tcp")
	s.openDebugForm(debugDraft{kind: debugServer, proto: debugTCP, port: strconv.Itoa(port), name: "echo", greeting: `hello\n`})
	tt.Frame()
	if err := tt.Click("创建"); err != nil {
		t.Fatal(err)
	}
	server := s.debug.sessions[0]
	if server.Kind != debugServer || server.Port != port || server.Greeting != `hello\n` {
		t.Fatalf("server saved as %+v", server)
	}
	r := s.debugRunOf(server.ID)
	settle(t, tt, "listening", func() bool { return r.snapshot().state == debugListening })
	if !tt.HasText("监听中") {
		t.Fatal("the server's state is not shown")
	}
	s.updateDebug(server.ID, func(y *debugSession) { y.AutoReply = true })

	s.openDebugForm(debugDraft{kind: debugClient, proto: debugTCP, byAddress: true, address: "127.0.0.1:" + strconv.Itoa(port), name: "client"})
	tt.Frame()
	if err := tt.Click("创建"); err != nil {
		t.Fatal(err)
	}
	client := s.debug.sessions[1]
	cr := s.debugRunOf(client.ID)
	settle(t, tt, "greeting", func() bool { return received("hello\n")(cr.snapshot()) })
	if !tt.HasText(`hello\n`) {
		t.Fatal("the greeting is not in the log")
	}
	// Type and send with the button; the server echoes.
	s.debugInputOf(client.ID).text = `PING\r\n`
	tt.Frame()
	if err := tt.Click("发送"); err != nil {
		t.Fatal(err)
	}
	settle(t, tt, "echo", func() bool { return received("PING\r\n")(cr.snapshot()) })
	if s.debugInputOf(client.ID).text != "" {
		t.Fatal("the composer kept the sent text")
	}
	// Hex view shows the bytes.
	s.debug.showHex[client.ID] = true
	tt.Frame()
	if !tt.HasText("50 49 4e 47 0d 0a") {
		t.Fatal("the hex view does not show the bytes")
	}
	// Bad hex input is refused with a message.
	in := s.debugInputOf(client.ID)
	in.hex, in.text = true, "5"
	tt.Frame()
	if err := tt.Click("发送"); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	if !tt.HasText("16 进制输入有误") {
		t.Fatal("bad hex is not reported")
	}

	// The sessions survive a new window; servers that listen with the
	// window start by themselves.
	s.updateDebug(server.ID, func(y *debugSession) { y.AutoListen = true })
	r.stop()
	cr.stop()
	again := newDesktop(engineClient{})
	again.prefsFile = s.prefsFile
	d := again.debugState()
	again.debugAutoListen(true)
	defer func() {
		for _, r := range again.debug.runs {
			r.stop()
		}
	}()
	if len(d.sessions) != 2 || d.sessions[1].Address != "127.0.0.1:"+strconv.Itoa(port) {
		t.Fatalf("reloaded %+v", d.sessions)
	}
	r2 := again.debugRunOf(server.ID)
	deadline := time.Now().Add(5 * time.Second)
	for r2.snapshot().state != debugListening && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if r2.snapshot().state != debugListening {
		t.Fatal("the auto-listen server did not start with the window")
	}
}

func TestDebugServerPeersAndBroadcast(t *testing.T) {
	s := debugDesktop(t)
	port := freePort(t, "tcp")
	x := debugSession{ID: "srv", Kind: debugServer, Proto: debugTCP, Name: "srv", Port: port}
	s.debugState().sessions = []debugSession{x}
	s.debug.cur = x.ID
	tt := ui.NewTester(s.view, 1180, 760)
	s.startDebug(x)
	r := s.debugRunOf(x.ID)
	settle(t, tt, "listening", func() bool { return r.snapshot().state == debugListening })
	var clients []*debugRun
	for range 2 {
		c := newDebugRun(nil)
		c.start(s.ctx, debugSession{Kind: debugClient, Proto: debugTCP, Address: "127.0.0.1:" + strconv.Itoa(port)}, directDial)
		defer c.stop()
		clients = append(clients, c)
	}
	settle(t, tt, "two peers", func() bool { return len(r.snapshot().peers) == 2 })
	if !tt.HasText("连接 2") {
		t.Fatal("the peers are not counted")
	}
	// The first is picked by itself; picking the second sends to both.
	second := r.snapshot().peers[1]
	r.pick(second.Addr, true)
	tt.Frame()
	if !tt.HasText("群发 2 个连接") {
		t.Fatal("the composer does not say it sends to both")
	}
	s.debugInputOf(x.ID).text = "PUSH"
	tt.Frame()
	if err := tt.Click("发送"); err != nil {
		t.Fatal(err)
	}
	for _, c := range clients {
		waitFor(t, c, "broadcast", received("PUSH"))
	}
}

func TestDebugLogCopiesCompletePacket(t *testing.T) {
	s := debugDesktop(t)
	x := debugSession{ID: "udp", Kind: debugClient, Proto: debugUDP, Name: "UDP"}
	r := s.debugRunOf(x.ID)
	s.debug.sessions = []debugSession{x}
	r.note("connected")
	r.add(debugLine{Dir: lineSent, Data: []byte("sent\x00\xff")})
	r.add(debugLine{Dir: lineReceived, Data: []byte(strings.Repeat("reply\x00\xff", 32))})
	tt := ui.NewTester(s.view, 1180, 760)
	const copyLabel = "复制数据包（16 进制）"

	for _, tc := range []struct {
		filter string
		hex    bool
		want   string
	}{
		{filter: "sent", want: "73 65 6e 74 00 ff"},
		{filter: "reply", hex: true, want: strings.TrimSpace(strings.Repeat("72 65 70 6c 79 00 ff ", 32))},
	} {
		s.debug.filter = tc.filter
		s.debug.showHex[x.ID] = tc.hex
		tt.Frame()
		if err := tt.Click(copyLabel); err != nil {
			t.Fatal(err)
		}
		if got := tt.Clipboard(); got != tc.want {
			t.Fatalf("clipboard for %s = %q, want %q", tc.filter, got, tc.want)
		}
	}

	s.debug.filter = "connected"
	tt.Frame()
	if !tt.HasText("connected") {
		t.Fatal("the connection event is missing")
	}
	if _, ok := tt.Access(copyLabel); ok {
		t.Fatal("the connection event has a packet copy button")
	}
}

func TestDebugDialChoosesPublishedPort(t *testing.T) {
	s := debugDesktop(t)
	s.containers = []container{{ID: "c1", Name: "redis", State: "running", Ports: []portMap{{Private: 6379, Public: 16379, Type: "tcp"}}}}
	if _, err := s.debugDial(debugSession{Kind: debugClient, Proto: debugTCP, Container: "redis", Port: 6379, Via: "host"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.debugDial(debugSession{Kind: debugClient, Proto: debugTCP, Container: "redis", Port: 7000, Via: "host"}); err == nil {
		t.Fatal("an unpublished port was dialed on the host")
	}
	if _, err := s.debugDial(debugSession{Kind: debugClient, Proto: debugTCP, Container: "gone", Port: 1, Via: "net"}); err == nil {
		t.Fatal("a missing container was dialed")
	}
	s.containers[0].State = "exited"
	if _, err := s.debugDial(debugSession{Kind: debugClient, Proto: debugTCP, Container: "redis", Port: 6379, Via: "net"}); err == nil {
		t.Fatal("a stopped container was dialed")
	}
}

func TestDebugScreenshots(t *testing.T) {
	dir := os.Getenv("FABO_SHOTS")
	if dir == "" {
		t.Skip("set FABO_SHOTS to a directory to render the pages")
	}
	for _, theme := range []string{"light", "dark"} {
		s := debugDesktop(t)
		s.prefs.Theme = theme
		tt := ui.NewTester(s.view, 1180, 760)
		shot := func(name string) {
			tt.Frame()
			file, err := os.Create(filepath.Join(dir, fmt.Sprintf("debug-%s-%s.png", name, theme)))
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			if err := png.Encode(file, tt.Image()); err != nil {
				t.Fatal(err)
			}
		}
		shot("empty")
		tcp, udp, mock := freePort(t, "tcp"), freePort(t, "udp"), freePort(t, "tcp")
		sessions := []debugSession{
			{ID: "srv", Kind: debugServer, Proto: debugTCP, Name: "设备上报", Port: tcp, AutoReply: true, Reply: `ACK\n`},
			{ID: "cli", Kind: debugClient, Proto: debugTCP, Name: "设备模拟", Address: "127.0.0.1:" + strconv.Itoa(tcp), Quick: []string{`PING\r\n`, `STATUS\r\n`}},
			{ID: "udp", Kind: debugServer, Proto: debugUDP, Name: "日志收集", Port: udp},
			{ID: "mock", Kind: debugServer, Proto: debugHTTP, Name: "支付网关 Mock", Port: mock, Routes: []debugRoute{
				{Method: "POST", Path: "/v1/charges", Status: 201, ContentType: "application/json", Body: `{"id":"ch_{{rand}}","status":"succeeded"}`},
				{Method: "GET", Path: "/v1/charges/:id", Status: 200, Body: `{"id":"{{path.id}}"}`},
				{Method: "POST", Path: "/v1/refunds", Status: 503, Body: `{"error":"timeout"}`, Delay: 50},
			}},
			{ID: "api", Kind: debugClient, Proto: debugHTTP, Name: "支付 API", Method: "POST", Address: "http://127.0.0.1:" + strconv.Itoa(mock) + "/v1/charges",
				Headers: []debugHeader{{"Content-Type", "application/json"}}, Body: `{"amount":259}`},
		}
		s.debug.sessions = sessions
		for _, x := range sessions[:4] {
			s.startDebug(x)
		}
		cli := s.debugRunOf("cli")
		settle(t, tt, "client", func() bool { return cli.snapshot().state == debugOpen })
		for _, m := range []string{"PING\r\n", "STATUS\r\n", "REPORT temp=21.5\r\n"} {
			_ = cli.sendClient([]byte(m), false)
			time.Sleep(30 * time.Millisecond)
		}
		settle(t, tt, "acks", func() bool { return len(cli.snapshot().lines) >= 7 })
		s.debug.cur = "cli"
		shot("client")
		s.debug.showHex["cli"] = true
		shot("client-hex")
		s.debug.cur = "srv"
		shot("server")
		s.startDebug(sessions[4])
		settle(t, tt, "exchange", func() bool { return len(s.debugRunOf("api").snapshot().exchanges) == 1 })
		s.debug.cur = "api"
		shot("http-client")
		s.debug.cur = "mock"
		shot("mock")
		s.openDebugForm(debugDraft{})
		shot("new")
		s.form = form{}
		s.openDebugForm(debugDraft{kind: debugServer, proto: debugWS, port: "9001", tls: true})
		shot("new-server")
		s.form = form{}
	}
}

// TestDebugMenuTakesOverSidebar checks that the debug page swaps the
// sidebar for its own menu, open and folded, and that its back button
// returns to the page the debug page was opened from.
func TestDebugMenuTakesOverSidebar(t *testing.T) {
	_, s := newFakeEngine(t)
	s.prefs.Lang = "zh"
	s.prefsFile = filepath.Join(t.TempDir(), "desktop.json")
	s.navigate(containersPage)
	s.navigate(debugPage)
	s.debugState().sessions = []debugSession{{ID: "a", Name: "设备模拟", Kind: debugClient, Proto: debugTCP, Port: 9}}
	tt := ui.NewTester(s.view, 1180, 760)
	tt.Frame()
	if !tt.HasText("设备模拟") || tt.HasText("镜像") {
		t.Fatalf("the debug menu does not replace the sidebar: %q", tt.Texts())
	}
	// Folded, the sessions stay reachable as badges.
	tt.SetSize(900, 760)
	tt.Frame()
	if !tt.HasText("TCP") {
		t.Fatalf("the folded debug menu lost its sessions: %q", tt.Texts())
	}
	if err := tt.Click("返回"); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	if s.page != containersPage {
		t.Fatalf("back went to %q, want %q", s.page, containersPage)
	}
}
