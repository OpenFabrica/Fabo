package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"
)

// The window's USB page on Windows. WSL 2, where the engine runs, gets USB
// devices through usbipd-win: a device is shared once (`usbipd bind`, which
// needs an administrator, so Windows asks) and then attached to WSL
// (`usbipd attach --wsl`), where containers see it as on any Linux machine.
// Upstream: crates/kiri-win/src/usb.rs.
//
// usbipd is the only source of truth: every list and every action reads
// `usbipd state` again, and fabo-win keeps nothing but when it attached
// each device, for the page's Attached column.

// usbDevice is one row of the USB page.
type usbDevice struct {
	BusID string `json:"busid"`
	Name  string `json:"name"`
	// ID is vendor:product in lowercase hex, "" when Windows' instance ID
	// does not carry it.
	ID     string `json:"id,omitempty"`
	Serial string `json:"serial,omitempty"`
	// InFabo: attached to WSL, so gone from Windows.
	InFabo bool `json:"inFabo"`
	// Shared: bound, so attaching it needs no administrator.
	Shared bool `json:"shared"`
	// Since is when this fabo-win attached it (Unix seconds); 0 when it was
	// attached before fabo-win started, or some other way.
	Since int64 `json:"since,omitempty"`
}

// usbView is GET /usb on the control pipe.
type usbView struct {
	// Missing: usbipd-win is not installed.
	Missing bool        `json:"missing,omitempty"`
	Error   string      `json:"error,omitempty"`
	Devices []usbDevice `json:"devices"`
	// Busy are the bus IDs with an action under way, and "install" while
	// usbipd-win installs, so a window opened meanwhile shows them waiting.
	Busy []string `json:"busy,omitempty"`
}

// usbResult answers a USB action that ended without an error.
type usbResult struct {
	// Name is the device's, for the window's message.
	Name string `json:"name,omitempty"`
	// Cancelled: the user declined Windows' administrator prompt. Nothing
	// changed, and the window says nothing.
	Cancelled bool `json:"cancelled,omitempty"`
}

// errUSBCancelled is the administrator prompt for `usbipd bind` declined.
var errUSBCancelled = errors.New("cancelled")

// errBadRequest is a request the control API refuses outright (400).
var errBadRequest = errors.New("bad request")

// errUSBNoEngine refuses an attach while the engine is not running: WSL
// may not be up to take the device, and no container could use it.
var errUSBNoEngine = errors.New("Fabo's engine isn't running: start it, then attach the device")

// errUSBMissing is an action that needs usbipd-win without it.
var errUSBMissing = errors.New("usbipd-win is not installed")

// usbSystem is what the USB page needs from Windows; tests fake it.
type usbSystem interface {
	// usbipdInstalled reports whether usbipd-win is installed.
	usbipdInstalled() bool
	// usbipd runs usbipd quietly and returns its output; a failure carries
	// usbipd's own message (usbipdMessage).
	usbipd(ctx context.Context, args ...string) ([]byte, error)
	// usbipdElevated runs usbipd as administrator, after Windows asks, and
	// returns its exit code; errUSBCancelled when the user declines.
	usbipdElevated(args ...string) (uint32, error)
	// engineRunning reports whether the engine in WSL answers.
	engineRunning() bool
	// installUSBIPD installs usbipd-win with winget.
	installUSBIPD(ctx context.Context) error
}

// usbManager serves the USB page. Actions on different devices may run at
// once; a second action on a device already busy is refused, and lists
// never wait for an action (one may sit on an administrator prompt).
type usbManager struct {
	sys usbSystem
	now func() time.Time

	mu    sync.Mutex
	since map[string]int64
	busy  map[string]bool
}

// newUSBManager returns the USB page's manager over sys.
func newUSBManager(sys usbSystem) *usbManager {
	return &usbManager{sys: sys, now: time.Now, since: map[string]int64{}, busy: map[string]bool{}}
}

// state is the page's list.
func (u *usbManager) state(ctx context.Context) usbView {
	v := usbView{Devices: []usbDevice{}}
	u.mu.Lock()
	for k := range u.busy {
		v.Busy = append(v.Busy, k)
	}
	u.mu.Unlock()
	slices.Sort(v.Busy)
	if !u.sys.usbipdInstalled() {
		v.Missing = true
		return v
	}
	devices, err := u.read(ctx)
	if err != nil {
		v.Error = err.Error()
		return v
	}
	v.Devices = devices
	return v
}

// read lists the devices from `usbipd state`, with when each was attached.
// A device no longer in Fabo forgets its time, unless the time is newer
// than this read: an attach that finished while usbipd answered would
// otherwise lose it to the stale list.
func (u *usbManager) read(ctx context.Context) ([]usbDevice, error) {
	started := u.now().Unix()
	out, err := u.sys.usbipd(ctx, "state")
	if err != nil {
		return nil, err
	}
	devices, err := parseUSBIPDState(out)
	if err != nil {
		return nil, fmt.Errorf("usbipd state: %w", err)
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	in := map[string]bool{}
	for i, d := range devices {
		if d.InFabo {
			in[d.BusID] = true
			devices[i].Since = u.since[d.BusID]
		}
	}
	for bus, at := range u.since {
		if !in[bus] && at < started {
			delete(u.since, bus)
		}
	}
	return devices, nil
}

// find reads the devices again and returns the one at bus.
func (u *usbManager) find(ctx context.Context, bus string) (usbDevice, error) {
	devices, err := u.read(ctx)
	if err != nil {
		return usbDevice{}, err
	}
	for _, d := range devices {
		if d.BusID == bus {
			return d, nil
		}
	}
	return usbDevice{}, fmt.Errorf("the USB device at %s is gone", bus)
}

// claim marks key busy, or fails with errBusy when it already is.
func (u *usbManager) claim(key string) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.busy[key] {
		return errBusy
	}
	u.busy[key] = true
	return nil
}

// release ends key's action.
func (u *usbManager) release(key string) {
	u.mu.Lock()
	delete(u.busy, key)
	u.mu.Unlock()
}

// action runs "attach" or "detach" on the device at bus, or "install".
func (u *usbManager) action(ctx context.Context, action, bus string) (usbResult, error) {
	switch action {
	case "attach", "detach":
		if !validBusID(bus) {
			return usbResult{}, fmt.Errorf("%w: bad bus ID %q", errBadRequest, bus)
		}
	case "install":
		bus = "install"
	default:
		return usbResult{}, errUnknownAction
	}
	if err := u.claim(bus); err != nil {
		return usbResult{}, err
	}
	defer u.release(bus)
	switch action {
	case "attach":
		return u.attach(ctx, bus)
	case "detach":
		return u.detach(ctx, bus)
	}
	return usbResult{}, u.install(ctx)
}

// attach hands the device at bus to WSL. Everything that can fail cheaply
// is checked before the administrator prompt, so the user never approves
// a bind that is bound to go nowhere. A bound device that then fails to
// attach stays bound: that is usbipd's "shared" state, not a half attach,
// and the next try needs no prompt.
func (u *usbManager) attach(ctx context.Context, bus string) (usbResult, error) {
	if !u.sys.usbipdInstalled() {
		return usbResult{}, errUSBMissing
	}
	d, err := u.find(ctx, bus)
	if err != nil {
		return usbResult{}, err
	}
	res := usbResult{Name: d.Name}
	if d.InFabo {
		return res, nil
	}
	if !u.sys.engineRunning() {
		return usbResult{}, errUSBNoEngine
	}
	if !d.Shared {
		code, err := u.sys.usbipdElevated("bind", "--busid", bus)
		switch {
		case errors.Is(err, errUSBCancelled):
			return usbResult{Name: d.Name, Cancelled: true}, nil
		case err != nil:
			return usbResult{}, err
		case code != 0:
			return usbResult{}, fmt.Errorf("usbipd could not share %s (exit %d)", d.Name, code)
		}
		// The prompt may have waited long; a window that gave up meanwhile
		// gets no attach it no longer expects.
		if err := ctx.Err(); err != nil {
			return usbResult{}, err
		}
	}
	if _, err := u.sys.usbipd(ctx, "attach", "--wsl", "--busid", bus); err != nil {
		// usbipd may fail after all (or report a failure that is not):
		// the device's state now is the answer.
		if now, e := u.find(context.WithoutCancel(ctx), bus); e != nil || !now.InFabo {
			return usbResult{}, fmt.Errorf("%s was not attached: %w", d.Name, err)
		}
	}
	u.mu.Lock()
	u.since[bus] = u.now().Unix()
	u.mu.Unlock()
	return res, nil
}

// detach gives the device at bus back to Windows. It stays shared.
func (u *usbManager) detach(ctx context.Context, bus string) (usbResult, error) {
	if !u.sys.usbipdInstalled() {
		return usbResult{}, errUSBMissing
	}
	d, err := u.find(ctx, bus)
	if err != nil {
		return usbResult{}, err
	}
	res := usbResult{Name: d.Name}
	if !d.InFabo {
		return res, nil
	}
	if _, err := u.sys.usbipd(ctx, "detach", "--busid", bus); err != nil {
		if now, e := u.find(context.WithoutCancel(ctx), bus); e != nil || now.InFabo {
			return usbResult{}, fmt.Errorf("%s was not detached: %w", d.Name, err)
		}
	}
	u.mu.Lock()
	delete(u.since, bus)
	u.mu.Unlock()
	return res, nil
}

// install installs usbipd-win. Its installer asks for approval itself.
// Whether usbipd is there afterwards is the answer, whatever winget said.
func (u *usbManager) install(ctx context.Context) error {
	if u.sys.usbipdInstalled() {
		return nil
	}
	err := u.sys.installUSBIPD(ctx)
	if u.sys.usbipdInstalled() {
		return nil
	}
	if err != nil {
		return err
	}
	return errors.New("usbipd-win was not installed")
}

// usbipdState is the part of `usbipd state`'s JSON the page uses.
type usbipdState struct {
	Devices []struct {
		BusID           *string `json:"BusId"`
		ClientIPAddress *string `json:"ClientIPAddress"`
		Description     string  `json:"Description"`
		InstanceID      string  `json:"InstanceId"`
		PersistedGUID   *string `json:"PersistedGuid"`
	} `json:"Devices"`
}

// parseUSBIPDState turns `usbipd state` into the page's rows. Devices
// usbipd remembers but that are not plugged in have no bus ID and nothing
// to attach, so they are left out.
func parseUSBIPDState(out []byte) ([]usbDevice, error) {
	var st usbipdState
	if err := json.Unmarshal(out, &st); err != nil {
		return nil, err
	}
	devices := []usbDevice{}
	for _, d := range st.Devices {
		if d.BusID == nil || *d.BusID == "" {
			continue
		}
		name := d.Description
		if name == "" {
			name = "USB device"
		}
		devices = append(devices, usbDevice{
			BusID: *d.BusID, Name: name, ID: usbInstanceID(d.InstanceID), Serial: usbInstanceSerial(d.InstanceID),
			InFabo: d.ClientIPAddress != nil, Shared: d.PersistedGUID != nil,
		})
	}
	return devices, nil
}

// usbInstanceID is vendor:product from an instance ID such as
// USB\VID_10C4&PID_EA60\0001.
func usbInstanceID(instance string) string {
	hex := func(key string) string {
		for _, part := range strings.FieldsFunc(strings.ToUpper(instance), func(r rune) bool { return r == '\\' || r == '&' }) {
			if v, ok := strings.CutPrefix(part, key); ok {
				return strings.ToLower(v)
			}
		}
		return ""
	}
	vid, pid := hex("VID_"), hex("PID_")
	if vid == "" || pid == "" {
		return ""
	}
	return vid + ":" + pid
}

// usbInstanceSerial is the device's own serial number: the instance ID's
// last part, unless Windows made one up (it has an & in it then).
func usbInstanceSerial(instance string) string {
	i := strings.LastIndex(instance, `\`)
	if i < 0 {
		return ""
	}
	serial := instance[i+1:]
	if strings.Contains(serial, "&") || strings.HasPrefix(strings.ToUpper(serial), "VID_") {
		return ""
	}
	return serial
}

// validBusID accepts usbipd's bus IDs, "<bus>-<port>", and nothing that
// could pass for another argument.
func validBusID(bus string) bool {
	parts := strings.Split(bus, "-")
	if len(bus) > 16 || len(parts) != 2 {
		return false
	}
	for _, p := range parts {
		if p == "" || strings.Trim(p, "0123456789") != "" {
			return false
		}
	}
	return true
}

// usbipdMessage is usbipd's reason for a failure: stderr, else stdout,
// without its "usbipd: error: " prefix.
func usbipdMessage(stdout, stderr []byte) string {
	text := strings.TrimSpace(string(stderr))
	if text == "" {
		text = strings.TrimSpace(string(stdout))
	}
	text = strings.TrimPrefix(text, "usbipd: ")
	text = strings.TrimPrefix(text, "error: ")
	return strings.TrimSpace(text)
}

// lastLine is the last non-empty line of out, for a tool's failure.
func lastLine(out []byte) string {
	lines := nonEmptyLines(string(out))
	if len(lines) == 0 {
		return ""
	}
	return lines[len(lines)-1]
}
