package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/egoist/mygo/ui"
)

// fakeUSBHost is fabo-win's control API with its USB endpoints, answering
// like engine/cmd/fabo-win/control.go.
type fakeUSBHost struct {
	mu      sync.Mutex
	view    hostUSB
	actions []string
	// cancel makes attach answer as if the administrator prompt was declined.
	cancel bool
	fail   string
}

// serve answers GET /status, GET /usb and the USB actions.
func (f *fakeUSBHost) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/status":
		json.NewEncoder(w).Encode(hostStatus{Engine: "running"})
	case r.Method == http.MethodGet && r.URL.Path == "/update":
		json.NewEncoder(w).Encode(hostUpdate{State: "idle"})
	case r.Method == http.MethodGet && r.URL.Path == "/usb":
		json.NewEncoder(w).Encode(f.view)
	case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/usb/"):
		body, _ := io.ReadAll(r.Body)
		var v struct{ Value string }
		json.Unmarshal(body, &v)
		action := strings.TrimPrefix(r.URL.Path, "/usb/")
		f.actions = append(f.actions, action+" "+v.Value)
		if f.fail != "" {
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]string{"message": f.fail})
			return
		}
		res := hostUSBResult{Cancelled: f.cancel}
		for i, d := range f.view.Devices {
			if d.BusID == v.Value {
				res.Name = d.Name
				if !f.cancel {
					f.view.Devices[i].InFabo = action == "attach"
				}
			}
		}
		if action == "install" {
			f.view.Missing = false
		}
		json.NewEncoder(w).Encode(res)
	default:
		http.NotFound(w, r)
	}
}

// newUSBDesktop is a window on the USB page with a fake fabo-win.
func newUSBDesktop(t *testing.T, view hostUSB) (*desktop, *fakeUSBHost) {
	t.Helper()
	f := &fakeUSBHost{view: view}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	addr := strings.TrimPrefix(srv.URL, "http://")
	_, s := newFakeEngine(t)
	s.prefs.Lang = "en"
	s.host = newHostClient(func(ctx context.Context) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", addr)
	})
	s.refreshHost()
	s.navigate(usbPage)
	return s, f
}

// sampleUSB is a PC with a serial adapter on Windows and a mouse in Fabo.
func sampleUSB() hostUSB {
	return hostUSB{Devices: []hostUSBDevice{
		{BusID: "1-3", Name: "CP210x USB to UART Bridge (COM3)", ID: "10c4:ea60", Serial: "0001"},
		{BusID: "2-1", Name: "USB Input Device", ID: "046d:c077", InFabo: true, Shared: true},
	}}
}

// TestUSBPageAttachAndDetach lists the devices and moves them.
func TestUSBPageAttachAndDetach(t *testing.T) {
	s, f := newUSBDesktop(t, sampleUSB())
	tt := ui.NewTester(s.view, 1180, 760)
	tt.Frame()
	for _, text := range []string{"CP210x USB to UART Bridge (COM3)", "On this PC", "In Fabo", "10c4:ea60", "2 devices"} {
		if !tt.HasText(text) {
			t.Fatalf("%q not shown", text)
		}
	}
	if err := tt.Click("Hand to Fabo's containers: CP210x USB to UART Bridge (COM3)"); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	if err := tt.Click("Give back to this PC: USB Input Device"); err != nil {
		t.Fatal(err)
	}
	tt.Advance(0)
	f.mu.Lock()
	got := strings.Join(f.actions, "; ")
	f.mu.Unlock()
	if got != "attach 1-3; detach 2-1" {
		t.Fatalf("actions = %s", got)
	}
	if !tt.HasText("CP210x USB to UART Bridge (COM3) is in Fabo") && !strings.Contains(strings.Join(s.notices, "|"), "is in Fabo") {
		t.Fatalf("no attach message; notices %v", s.notices)
	}
}

// TestUSBPageDeclinedAndFailed stays quiet when the prompt is declined and
// shows fabo-win's reason when an action fails.
func TestUSBPageDeclinedAndFailed(t *testing.T) {
	s, f := newUSBDesktop(t, sampleUSB())
	f.cancel = true
	s.usbAct("attach", "1-3", "CP210x")
	if len(s.notices) != 0 || s.working["usb:1-3"] {
		t.Fatalf("declined: notices %v, working %v", s.notices, s.working)
	}
	f.mu.Lock()
	f.cancel, f.fail = false, "CP210x was not attached: There is no WSL 2 distribution running."
	f.mu.Unlock()
	s.usbAct("attach", "1-3", "CP210x")
	if len(s.notices) != 1 || s.notices[0] != "CP210x was not attached: There is no WSL 2 distribution running." {
		t.Fatalf("failed: notices %v", s.notices)
	}
}

// TestUSBPageMissing offers to install usbipd-win, and the page is a
// placeholder without fabo-win or the launcher.
func TestUSBPageMissing(t *testing.T) {
	s, f := newUSBDesktop(t, hostUSB{Missing: true, Devices: []hostUSBDevice{}})
	tt := ui.NewTester(s.view, 1180, 760)
	tt.Frame()
	if !tt.HasText("USB needs usbipd-win") {
		t.Fatal("missing state not shown")
	}
	if err := tt.Click("Install usbipd-win"); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	got := strings.Join(f.actions, "; ")
	f.mu.Unlock()
	if got != "install " {
		t.Fatalf("actions = %q", got)
	}

	_, plain := newFakeEngine(t)
	plain.prefs.Lang = "en"
	plain.navigate(usbPage)
	tt = ui.NewTester(plain.view, 1180, 760)
	tt.Frame()
	if !tt.HasText("coming soon") {
		t.Fatal("USB page is not a placeholder without fabo-win or the launcher")
	}
}

// TestUSBPageWhileEngineStopped keeps the page, so a device left in WSL
// can be given back with the engine stopped.
func TestUSBPageWhileEngineStopped(t *testing.T) {
	s, f := newUSBDesktop(t, sampleUSB())
	s.info.FaboStopped = true
	tt := ui.NewTester(s.view, 1180, 760)
	tt.Frame()
	if tt.HasText("The engine is stopped") {
		t.Fatal("engine placeholder hides the USB page")
	}
	if err := tt.Click("Give back to this PC: USB Input Device"); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if strings.Join(f.actions, "; ") != "detach 2-1" {
		t.Fatalf("actions = %v", f.actions)
	}
}

// macUSBListing is `fabo usb list --json` on a Mac, as engine/cmd/fabo's
// TestMacUSBRows prints it (usbListGolden there): the contract between the
// launcher and the window.
const macUSBListing = `{"can":"attach","devices":[{"busid":"0x01100000","name":"CP2102 USB to UART","id":"10c4:ea60","kind":"serial","speed":"full","inFabo":true,"since":1763370000},{"busid":"0x01200000","name":"Keyboard","id":"05ac:024f","speed":"low","inFabo":false},{"busid":"fake-serial","name":"Fabo fake serial","id":"","inFabo":true,"since":1763370100}]}`

// fakeUSBLauncher is the `fabo` command's USB part, recording the moves.
type fakeUSBLauncher struct {
	mu      sync.Mutex
	listing string
	calls   []string
	fail    error
}

// run answers `fabo usb list --json` and records attach and detach.
func (l *fakeUSBLauncher) run(_ context.Context, args ...string) ([]byte, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(args) == 3 && args[0] == "usb" && args[1] == "list" && args[2] == "--json" {
		return []byte(l.listing), nil
	}
	if len(args) > 0 && args[0] == "usb" {
		l.calls = append(l.calls, strings.Join(args, " "))
		return nil, l.fail
	}
	return nil, nil
}

// newLauncherUSBDesktop is a window without fabo-win, on the USB page, whose
// launcher lists listing.
func newLauncherUSBDesktop(t *testing.T, listing string) (*desktop, *fakeUSBLauncher) {
	t.Helper()
	l := &fakeUSBLauncher{listing: listing}
	_, s := newFakeEngine(t)
	s.prefs.Lang = "en"
	s.launcher = l.run
	s.navigate(usbPage)
	s.refreshUSB()
	return s, l
}

// TestUSBOnMac lists the Mac's devices through the launcher, attaches and
// detaches by location, and stays quiet when the password prompt closes.
func TestUSBOnMac(t *testing.T) {
	s, l := newLauncherUSBDesktop(t, macUSBListing)
	tt := ui.NewTester(s.view, 1180, 760)
	tt.Frame()
	if tt.HasText("coming soon") || !tt.HasText("Keyboard") || !tt.HasText("On this Mac") || !tt.HasText("Fabo fake serial") {
		t.Fatal("the Mac's devices are not listed")
	}
	if !tt.HasText("A device in Fabo is gone from the Mac until you detach it.") {
		t.Fatal("Mac note missing")
	}
	if err := tt.Click("Hand to Fabo's containers: Keyboard"); err != nil {
		t.Fatal(err)
	}
	if err := tt.Click("Give back to the Mac: CP2102 USB to UART"); err != nil {
		t.Fatal(err)
	}
	l.mu.Lock()
	got := strings.Join(l.calls, "; ")
	l.fail = errors.New("canceled")
	l.mu.Unlock()
	if got != "usb attach 0x01200000; usb detach 0x01100000" {
		t.Fatalf("calls = %q", got)
	}
	s.notices = nil
	s.usbAct("attach", "0x01200000", "Keyboard")
	if len(s.notices) != 0 || s.working["usb:0x01200000"] {
		t.Fatalf("a closed password prompt said %v", s.notices)
	}
	l.mu.Lock()
	l.fail = errors.New("the USB helper is not answering")
	l.mu.Unlock()
	s.usbAct("attach", "0x01200000", "Keyboard")
	if len(s.notices) != 1 || !strings.Contains(s.notices[0], "not answering") {
		t.Fatalf("failure not shown: %v", s.notices)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.calls) != 4 {
		t.Fatalf("calls = %v", l.calls)
	}
}

// TestUSBOnLinux lists the machine's devices with their paths and no
// buttons: containers see them already.
func TestUSBOnLinux(t *testing.T) {
	s, l := newLauncherUSBDesktop(t, `{"can":"none","devices":[{"busid":"/dev/bus/usb/001/004","name":"CP2102","id":"10c4:ea60","path":"/dev/ttyUSB0","inFabo":false}]}`)
	tt := ui.NewTester(s.view, 1180, 760)
	tt.Frame()
	if !tt.HasText("/dev/ttyUSB0") || !tt.HasText("they see its USB devices as they are") {
		t.Fatal("Linux devices or note not shown")
	}
	if _, found := tt.Find("Attach"); found {
		t.Fatal("an Attach button on Linux")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.calls) != 0 {
		t.Fatalf("calls = %v", l.calls)
	}
}
