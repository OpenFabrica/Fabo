package main

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// usbipdFixture is `usbipd state` with a device on Windows (not shared),
// one in WSL (shared) and one remembered but unplugged.
const usbipdFixture = `{"Devices":[
 {"BusId":"1-3","ClientIPAddress":null,"Description":"CP210x USB to UART Bridge (COM3)","InstanceId":"USB\\VID_10C4&PID_EA60\\0001","IsForced":false,"PersistedGuid":null,"StubInstanceId":null},
 {"BusId":"2-1","ClientIPAddress":"172.20.0.2","Description":"USB Input Device","InstanceId":"USB\\VID_046D&PID_C077\\5&2A","IsForced":false,"PersistedGuid":"6a1c1d1e-0000-0000-0000-000000000000","StubInstanceId":"x"},
 {"BusId":null,"ClientIPAddress":null,"Description":"Unplugged","InstanceId":"USB\\VID_1111&PID_2222\\9","PersistedGuid":"7b"}]}`

// TestParseUSBIPDState reads usbipd's JSON as upstream's rows test does.
func TestParseUSBIPDState(t *testing.T) {
	d, err := parseUSBIPDState([]byte(usbipdFixture))
	if err != nil {
		t.Fatal(err)
	}
	if len(d) != 2 {
		t.Fatalf("devices = %+v", d)
	}
	if d[0].BusID != "1-3" || d[0].ID != "10c4:ea60" || d[0].InFabo || d[0].Shared || d[0].Serial != "0001" {
		t.Fatalf("first = %+v", d[0])
	}
	// No serial number when Windows made the instance's last part up.
	if d[1].ID != "046d:c077" || !d[1].InFabo || !d[1].Shared || d[1].Serial != "" {
		t.Fatalf("second = %+v", d[1])
	}
	if _, err := parseUSBIPDState([]byte("usbipd: error: nope")); err == nil {
		t.Fatal("non-JSON accepted")
	}
}

// TestValidBusID keeps anything but "<bus>-<port>" out of usbipd's arguments.
func TestValidBusID(t *testing.T) {
	for _, ok := range []string{"1-3", "12-10"} {
		if !validBusID(ok) {
			t.Errorf("%q refused", ok)
		}
	}
	for _, bad := range []string{"", "1-3 & calc", "--busid", "1-", "-3", "1-2-3", "a-1", "1234567890-1234567"} {
		if validBusID(bad) {
			t.Errorf("%q accepted", bad)
		}
	}
}

// TestUSBIPDMessage keeps usbipd's reason without its prefix.
func TestUSBIPDMessage(t *testing.T) {
	if got := usbipdMessage(nil, []byte("usbipd: error: There is no WSL 2 distribution running.\n")); got != "There is no WSL 2 distribution running." {
		t.Fatalf("stderr = %q", got)
	}
	if got := usbipdMessage([]byte("usbipd: error: Device is not shared.\n"), nil); got != "Device is not shared." {
		t.Fatalf("stdout = %q", got)
	}
}

// fakeUSB is usbipd-win on a fake PC: devices by bus ID, and a record of
// every command.
type fakeUSB struct {
	mu        sync.Mutex
	installed bool
	engine    bool
	shared    map[string]bool
	inFabo    map[string]bool
	plugged   []string
	calls     []string
	// What the next commands answer.
	bindCode    uint32
	bindErr     error
	attachErr   error
	attachTakes bool // attach changes the state even when it fails
	installs    bool
	// bindGate, when set, holds bind until it is closed.
	bindGate chan struct{}
}

// newFakeUSB is a PC with usbipd-win, the engine running and two devices
// plugged in, neither shared.
func newFakeUSB() *fakeUSB {
	return &fakeUSB{installed: true, engine: true, shared: map[string]bool{}, inFabo: map[string]bool{}, plugged: []string{"1-3", "2-1"}}
}

// usbipdInstalled reports the fake's installed flag.
func (f *fakeUSB) usbipdInstalled() bool { f.mu.Lock(); defer f.mu.Unlock(); return f.installed }

// engineRunning reports the fake's engine flag.
func (f *fakeUSB) engineRunning() bool { return f.engine }

// usbipd answers state from the fake's devices and records the rest.
func (f *fakeUSB) usbipd(_ context.Context, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, strings.Join(args, " "))
	switch args[0] {
	case "state":
		var rows []string
		for _, bus := range f.plugged {
			client, guid := "null", "null"
			if f.inFabo[bus] {
				client = `"172.20.0.2"`
			}
			if f.shared[bus] {
				guid = `"g"`
			}
			rows = append(rows, `{"BusId":"`+bus+`","ClientIPAddress":`+client+`,"Description":"Dev `+bus+`","InstanceId":"USB\\VID_10C4&PID_EA60\\`+bus+`","PersistedGuid":`+guid+`}`)
		}
		return []byte(`{"Devices":[` + strings.Join(rows, ",") + `]}`), nil
	case "attach":
		bus := args[len(args)-1]
		if f.attachErr != nil {
			f.inFabo[bus] = f.attachTakes
			return nil, f.attachErr
		}
		if !f.shared[bus] {
			return nil, errors.New("Device is not shared")
		}
		f.inFabo[bus] = true
	case "detach":
		f.inFabo[args[len(args)-1]] = false
	}
	return nil, nil
}

// usbipdElevated is bind behind a fake administrator prompt.
func (f *fakeUSB) usbipdElevated(args ...string) (uint32, error) {
	if f.bindGate != nil {
		<-f.bindGate
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "elevated "+strings.Join(args, " "))
	if f.bindErr != nil || f.bindCode != 0 {
		return f.bindCode, f.bindErr
	}
	f.shared[args[len(args)-1]] = true
	return 0, nil
}

// installUSBIPD installs when installs is set.
func (f *fakeUSB) installUSBIPD(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "winget")
	if !f.installs {
		return errors.New("usbipd-win was not installed: No package found")
	}
	f.installed = true
	return nil
}

// commands are the fake's calls other than state.
func (f *fakeUSB) commands() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.calls {
		if c != "state" {
			out = append(out, c)
		}
	}
	return out
}

// TestUSBAttachBindsOnceThenAttaches walks a device from Windows into
// Fabo and back, with the prompt only the first time.
func TestUSBAttachBindsOnceThenAttaches(t *testing.T) {
	f := newFakeUSB()
	u := newUSBManager(f)
	u.now = func() time.Time { return time.Unix(1000, 0) }
	ctx := context.Background()
	res, err := u.action(ctx, "attach", "1-3")
	if err != nil || res.Name != "Dev 1-3" || res.Cancelled {
		t.Fatalf("attach = %+v %v", res, err)
	}
	if got := strings.Join(f.commands(), "; "); got != "elevated bind --busid 1-3; attach --wsl --busid 1-3" {
		t.Fatalf("commands = %s", got)
	}
	v := u.state(ctx)
	if !v.Devices[0].InFabo || !v.Devices[0].Shared || v.Devices[0].Since != 1000 || v.Devices[1].InFabo {
		t.Fatalf("state = %+v", v)
	}
	// Attaching again is nothing to do.
	if _, err := u.action(ctx, "attach", "1-3"); err != nil || len(f.commands()) != 2 {
		t.Fatalf("second attach: %v %v", err, f.commands())
	}
	if _, err := u.action(ctx, "detach", "1-3"); err != nil {
		t.Fatal(err)
	}
	if v := u.state(ctx); v.Devices[0].InFabo || v.Devices[0].Since != 0 || !v.Devices[0].Shared {
		t.Fatalf("after detach = %+v", v.Devices[0])
	}
	// Shared now: no prompt this time.
	f.calls = nil
	if _, err := u.action(ctx, "attach", "1-3"); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(f.commands(), "; "); got != "attach --wsl --busid 1-3" {
		t.Fatalf("commands = %s", got)
	}
}

// TestUSBAttachFailures checks that each failure leaves the device on
// Windows (never half attached) and says why.
func TestUSBAttachFailures(t *testing.T) {
	ctx := context.Background()
	t.Run("declined", func(t *testing.T) {
		f := newFakeUSB()
		f.bindErr = errUSBCancelled
		res, err := newUSBManager(f).action(ctx, "attach", "1-3")
		if err != nil || !res.Cancelled {
			t.Fatalf("declined = %+v %v", res, err)
		}
		if got := f.commands(); len(got) != 1 || f.inFabo["1-3"] {
			t.Fatalf("commands = %v", got)
		}
	})
	t.Run("bind fails", func(t *testing.T) {
		f := newFakeUSB()
		f.bindCode = 1
		_, err := newUSBManager(f).action(ctx, "attach", "1-3")
		if err == nil || !strings.Contains(err.Error(), "could not share Dev 1-3 (exit 1)") || len(f.commands()) != 1 {
			t.Fatalf("bind exit = %v %v", err, f.commands())
		}
	})
	t.Run("attach fails", func(t *testing.T) {
		f := newFakeUSB()
		f.attachErr = errors.New("There is no WSL 2 distribution running.")
		u := newUSBManager(f)
		_, err := u.action(ctx, "attach", "1-3")
		if err == nil || err.Error() != "Dev 1-3 was not attached: There is no WSL 2 distribution running." {
			t.Fatalf("attach = %v", err)
		}
		// Bound, so the next try needs no prompt; not in Fabo, no time.
		if d := u.state(ctx).Devices[0]; d.InFabo || !d.Shared || d.Since != 0 {
			t.Fatalf("after = %+v", d)
		}
	})
	t.Run("attach reports a failure that is not", func(t *testing.T) {
		f := newFakeUSB()
		f.attachErr, f.attachTakes = errors.New("timed out"), true
		u := newUSBManager(f)
		if _, err := u.action(ctx, "attach", "1-3"); err != nil {
			t.Fatalf("attach = %v", err)
		}
		if d := u.state(ctx).Devices[0]; !d.InFabo || d.Since == 0 {
			t.Fatalf("after = %+v", d)
		}
	})
	t.Run("engine stopped", func(t *testing.T) {
		f := newFakeUSB()
		f.engine = false
		if _, err := newUSBManager(f).action(ctx, "attach", "1-3"); !errors.Is(err, errUSBNoEngine) || len(f.commands()) != 0 {
			t.Fatalf("engine stopped = %v %v", err, f.commands())
		}
	})
	t.Run("gone", func(t *testing.T) {
		f := newFakeUSB()
		if _, err := newUSBManager(f).action(ctx, "attach", "3-1"); err == nil || !strings.Contains(err.Error(), "gone") || len(f.commands()) != 0 {
			t.Fatalf("gone = %v %v", err, f.commands())
		}
	})
	t.Run("bad bus ID", func(t *testing.T) {
		f := newFakeUSB()
		if _, err := newUSBManager(f).action(ctx, "attach", "1-3 & calc"); !errors.Is(err, errBadRequest) || len(f.calls) != 0 {
			t.Fatalf("bad bus = %v %v", err, f.calls)
		}
	})
	t.Run("window gave up during the prompt", func(t *testing.T) {
		f := newFakeUSB()
		cctx, cancel := context.WithCancel(ctx)
		f.bindGate = make(chan struct{})
		go func() { cancel(); close(f.bindGate) }()
		if _, err := newUSBManager(f).action(cctx, "attach", "1-3"); !errors.Is(err, context.Canceled) || f.inFabo["1-3"] {
			t.Fatalf("gave up = %v", err)
		}
	})
}

// TestUSBBusyAndMissing covers a second action on a busy device, lists
// meanwhile, and usbipd-win missing then installed.
func TestUSBBusyAndMissing(t *testing.T) {
	ctx := context.Background()
	f := newFakeUSB()
	f.bindGate = make(chan struct{})
	u := newUSBManager(f)
	done := make(chan error)
	go func() { _, err := u.action(ctx, "attach", "1-3"); done <- err }()
	deadline := time.Now().Add(5 * time.Second)
	for !slices.Contains(u.state(ctx).Busy, "1-3") {
		if time.Now().After(deadline) {
			t.Fatal("attach never became busy")
		}
		time.Sleep(time.Millisecond)
	}
	// The prompt is open: the list still answers, the same device is busy,
	// another one is not.
	if _, err := u.action(ctx, "attach", "1-3"); !errors.Is(err, errBusy) {
		t.Fatalf("second attach = %v", err)
	}
	if _, err := u.action(ctx, "detach", "2-1"); err != nil {
		t.Fatalf("other device = %v", err)
	}
	close(f.bindGate)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if len(u.state(ctx).Busy) != 0 {
		t.Fatal("still busy")
	}

	m := newFakeUSB()
	m.installed = false
	u = newUSBManager(m)
	if v := u.state(ctx); !v.Missing || v.Error != "" {
		t.Fatalf("missing = %+v", v)
	}
	if _, err := u.action(ctx, "attach", "1-3"); !errors.Is(err, errUSBMissing) {
		t.Fatalf("attach without usbipd = %v", err)
	}
	if _, err := u.action(ctx, "install", ""); err == nil || !strings.Contains(err.Error(), "No package found") {
		t.Fatalf("install failed = %v", err)
	}
	m.installs = true
	if _, err := u.action(ctx, "install", ""); err != nil || u.state(ctx).Missing {
		t.Fatalf("install = %v", err)
	}
	if _, err := u.action(ctx, "eject", "1-3"); !errors.Is(err, errUnknownAction) {
		t.Fatalf("unknown = %v", err)
	}
}
