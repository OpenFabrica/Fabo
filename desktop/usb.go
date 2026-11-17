package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"runtime"
	"strings"
	"time"

	"github.com/egoist/mygo/ui"
)

// The USB page. On Windows fabo-win lists the PC's devices through
// usbipd-win and hands them to WSL, where the engine runs
// (engine/cmd/fabo-win/usb.go). On a Mac and on Linux the launcher does:
// `fabo usb list --json` in the same shape, and on a Mac `fabo usb
// attach|detach LOCATION`, which installs the USB helper behind a password
// prompt the first time. Linux containers already see the machine's
// devices, so there the page only lists them. Upstream: the USB view of
// crates/kiri/src/ui/index.html with crates/kiri/src/usb.rs (ui_list,
// ui_attach, ui_detach) and crates/kiri-win/src/usb.rs.

const usbPage = "usb"

// hostUSBDevice is one device of fabo-win's GET /usb, or of the
// launcher's `fabo usb list --json` (engine/cmd/fabo/usb.go's usbRow).
type hostUSBDevice struct {
	BusID  string `json:"busid"`
	Name   string `json:"name"`
	ID     string `json:"id"`
	Serial string `json:"serial"`
	InFabo bool   `json:"inFabo"`
	Shared bool   `json:"shared"`
	Since  int64  `json:"since"`
	// Speed and Kind come from the launcher on a Mac; Path on Linux is the
	// node to pass to --device.
	Speed string `json:"speed"`
	Kind  string `json:"kind"`
	Path  string `json:"path"`
}

// hostUSB is fabo-win's GET /usb, or `fabo usb list --json`.
type hostUSB struct {
	Missing bool            `json:"missing"`
	Error   string          `json:"error"`
	Devices []hostUSBDevice `json:"devices"`
	Busy    []string        `json:"busy"`
	// Can is the launcher's "attach" (a Mac) or "none" (Linux, where
	// containers already see the devices); fabo-win leaves it empty.
	Can string `json:"can"`
}

// hostUSBResult answers a USB action that did not fail.
type hostUSBResult struct {
	Name      string `json:"name"`
	Cancelled bool   `json:"cancelled"`
}

// usb reads the PC's USB devices.
func (h *hostClient) usb(ctx context.Context) (hostUSB, error) {
	var v hostUSB
	return v, h.call(ctx, http.MethodGet, "/usb", nil, &v)
}

// usbAction attaches or detaches the device at busID, or installs usbipd-win.
func (h *hostClient) usbAction(ctx context.Context, action, busID string) (hostUSBResult, error) {
	var r hostUSBResult
	var body any
	if busID != "" {
		body = map[string]string{"value": busID}
	}
	return r, h.call(ctx, http.MethodPost, "/usb/"+action, body, &r)
}

// usbUI is what the window knows about the USB devices.
type usbUI struct {
	view    hostUSB
	known   bool
	loading bool
	// next is the earliest next read: the page follows plugging in and out
	// within a few seconds without asking usbipd on every tick.
	next time.Time
	// problem is why the last read failed (fabo-win not answering).
	problem string
}

// hasUSB reports whether the window has a USB page: with fabo-win, or with
// the launcher beside it on a Mac or Linux.
func (s *desktop) hasUSB() bool { return s.host != nil || s.usbByLauncher() }

// usbByLauncher reports whether the devices come from `fabo usb` rather
// than fabo-win. Windows' fabo.exe hands USB to WSL's fabo, which cannot
// see the PC's devices, so there it is fabo-win or nothing.
func (s *desktop) usbByLauncher() bool {
	return s.host == nil && s.launcher != nil && runtime.GOOS != "windows"
}

// usbCanMove reports whether devices move in and out of Fabo here: not on
// Linux, where containers see them as they are.
func (s *desktop) usbCanMove() bool { return s.usb.view.Can != "none" }

// usbOnMac reports whether the devices are a Mac's: the launcher says it
// can attach them. Read from the listing rather than the build, so the
// wording follows the data in tests too.
func (s *desktop) usbOnMac() bool { return s.host == nil && s.usb.view.Can == "attach" }

// usbHere is where a device not in Fabo is, in the system's own words.
func (s *desktop) usbHere() string {
	if s.usbOnMac() {
		return tr("在这台 Mac 上", "On this Mac")
	}
	return tr("在这台电脑上", "On this PC")
}

// readUSB reads the devices from fabo-win or the launcher.
func (s *desktop) readUSB(ctx context.Context) (hostUSB, error) {
	if s.host != nil {
		return s.host.usb(ctx)
	}
	var v hostUSB
	out, err := s.launcher(ctx, "usb", "list", "--json")
	if err == nil {
		err = json.Unmarshal(out, &v)
	}
	return v, err
}

// moveUSB attaches or detaches a device, or installs usbipd-win, through
// fabo-win or the launcher. A closed password prompt on a Mac is the
// launcher's "canceled", a choice and not a failure, as for domains.
func (s *desktop) moveUSB(ctx context.Context, action, busID string) (hostUSBResult, error) {
	if s.host != nil {
		return s.host.usbAction(ctx, action, busID)
	}
	if _, err := s.launcher(ctx, "usb", action, busID); err != nil {
		if strings.Contains(err.Error(), "canceled") {
			return hostUSBResult{Cancelled: true}, nil
		}
		return hostUSBResult{}, err
	}
	return hostUSBResult{}, nil
}

// navSoon reports whether a sidebar entry is still a placeholder here.
func (s *desktop) navSoon(n navItem) bool {
	if n.page == usbPage {
		return !s.hasUSB()
	}
	return n.soon
}

// refreshUSB reads the devices again, at most every three seconds.
func (s *desktop) refreshUSB() {
	if !s.hasUSB() || s.usb.loading || time.Now().Before(s.usb.next) {
		return
	}
	s.usb.loading = true
	s.run(15*time.Second, func(ctx context.Context) func() {
		v, err := s.readUSB(ctx)
		return func() {
			s.usb.loading, s.usb.next = false, time.Now().Add(3*time.Second)
			if err != nil {
				s.usb.problem = err.Error()
				return
			}
			s.usb.view, s.usb.known, s.usb.problem = v, true, ""
		}
	})
	if s.engine.http == nil {
		s.usb.loading = false
	}
}

// usbBusy reports whether an action on key ("install" or a bus ID) is
// under way, from this window or, as fabo-win says, from an earlier one.
func (s *desktop) usbBusy(key string) bool {
	if s.working["usb:"+key] {
		return true
	}
	for _, b := range s.usb.view.Busy {
		if b == key {
			return true
		}
	}
	return false
}

// usbAct runs a USB action. Attaching a device the first time waits on
// Windows' administrator prompt, so the wait is long; declining it says
// nothing.
func (s *desktop) usbAct(action, busID, name string) {
	key := "usb:" + busID
	if action == "install" {
		key = "usb:install"
	}
	if !s.hasUSB() || s.working[key] {
		return
	}
	s.working[key] = true
	timeout := 5 * time.Minute
	if action == "install" {
		timeout = 20 * time.Minute
	}
	s.run(timeout, func(ctx context.Context) func() {
		res, err := s.moveUSB(ctx, action, busID)
		return func() {
			delete(s.working, key)
			s.usb.next = time.Time{}
			if res.Name != "" {
				name = res.Name
			}
			switch {
			case err != nil:
				s.notices = append(s.notices, err.Error())
			case res.Cancelled:
			case action == "attach":
				s.notices = append(s.notices, fmt.Sprintf(tr("%s 已交给 Fabo", "%s is in Fabo"), name))
			case action == "detach" && s.usbOnMac():
				s.notices = append(s.notices, fmt.Sprintf(tr("%s 已还给 Mac", "%s is back on the Mac"), name))
			case action == "detach":
				s.notices = append(s.notices, fmt.Sprintf(tr("%s 已还给 Windows", "%s is back on Windows"), name))
			default:
				s.notices = append(s.notices, tr("usbipd-win 已安装", "usbipd-win is installed"))
			}
			s.refreshUSB()
		}
	})
	if s.engine.http == nil {
		delete(s.working, key)
	}
}

// usbShown are the devices the search matches.
func (s *desktop) usbShown() []hostUSBDevice {
	query := strings.ToLower(strings.TrimSpace(s.search))
	var shown []hostUSBDevice
	for _, d := range s.usb.view.Devices {
		if strings.Contains(strings.ToLower(d.Name+" "+d.ID+" "+d.BusID+" "+d.Path), query) {
			shown = append(shown, d)
		}
	}
	return shown
}

// usbInFabo counts the devices attached to WSL.
func (s *desktop) usbInFabo() int {
	n := 0
	for _, d := range s.usb.view.Devices {
		if d.InFabo {
			n++
		}
	}
	return n
}

// usbView is the USB page: the PC's devices, which are in Fabo, and the
// button that moves each one.
func (s *desktop) usbView(c *ui.Context, p palette) {
	v := s.usb.view
	switch {
	case !s.usb.known && s.usb.problem != "":
		usbEmpty(c, p, tr("读不到 USB 设备", "Couldn't read the USB devices"), s.usb.problem, "", false)
		return
	case !s.usb.known:
		usbEmpty(c, p, tr("正在查找 USB 设备…", "Looking for USB devices…"), "", "", false)
		return
	case v.Missing:
		busy := s.usbBusy("install")
		action := tr("安装 usbipd-win", "Install usbipd-win")
		if busy {
			action = tr("正在安装…", "Installing…")
		}
		if usbEmpty(c, p, tr("USB 需要 usbipd-win", "USB needs usbipd-win"),
			tr("Windows 通过 usbipd-win（微软推荐的工具）把 USB 设备交给 Fabo 的容器所在的 WSL。", "Windows hands USB devices to WSL, where Fabo's containers run, through usbipd-win, Microsoft's recommended tool."),
			action, busy) {
			s.usbAct("install", "", "")
		}
		return
	case v.Error != "" && len(v.Devices) == 0:
		usbEmpty(c, p, tr("读不到 USB 设备", "Couldn't read the USB devices"), v.Error, "", false)
		return
	case len(v.Devices) == 0:
		usbEmpty(c, p, tr("没有 USB 设备", "No USB devices"),
			tr("插上一个设备：串口转接器、Zigbee 棒、单片机……它会出现在这里。", "Plug one in: a serial adapter, a Zigbee stick, a microcontroller. It shows up here."), "", false)
		return
	}
	shown := s.usbShown()
	toolbar(c, func() {
		ui.Text(c, fmt.Sprintf(tr("%d 个设备", "%d devices"), len(v.Devices))).FontSize(12).FontFeatures("tnum").TextColor(p.muted)
	})
	// Beside the panel only the name, the state and the button stay. On
	// Linux nothing moves: the device's path takes the state's place.
	narrow := s.narrow()
	cols := narrowColumns([]column{{tr("设备", "Device"), 0, 1, false}, {"ID", 96, 0, false}, {tr("状态", "State"), 120, 0, false},
		{tr("接入时间", "Attached"), 86, 0, false}}, narrow, 1, 3)
	actionsWidth := float32(120)
	if !s.usbCanMove() {
		cols = narrowColumns([]column{{tr("设备", "Device"), 0, 1, false}, {"ID", 96, 0, false}, {tr("路径", "Path"), 200, 0, false}}, narrow, 1, 2)
		actionsWidth = 0
	}
	if !s.dockMax {
		tableHeader(c, p, cols, actionsWidth, nil)
		if len(shown) == 0 {
			empty(c, p, "usb", tr("没有匹配的设备", "No matching devices"), "")
		} else {
			ui.List(c.Key("rows"), s.list(usbPage), len(shown)).FillWidth().Grow(1).MinHeight(0).
				ItemKey(func(i int) any { return shown[i].BusID }).
				Rows(func(r ui.ListRow) { s.usbRow(r.Context, p, narrow, actionsWidth, shown[r.Index]) })
		}
		ui.Text(c, usbNote(s.usbCanMove(), s.usbOnMac())).
			FontSize(12).TextColor(p.muted).Padding(12, pageInset, 14, pageInset).Shrink(0)
	}
	for _, d := range v.Devices {
		if d.BusID == s.selected[usbPage] {
			s.usbDock(c, p, d)
		}
	}
}

// usbNote is the line under the table: where a device in Fabo goes, or on
// Linux that containers see the devices already.
func usbNote(canMove, mac bool) string {
	switch {
	case !canMove:
		return tr("容器直接运行在这台电脑的内核上，能原样看到它的 USB 设备：docker run --device 加上显示的路径。",
			"Containers run on this computer's own kernel, so they see its USB devices as they are: docker run --device with the path shown.")
	case mac:
		return tr("交给 Fabo 的设备在 Mac 上会消失，直到你把它还回去。在容器里使用：--device /dev/ttyUSB0（串口）、/dev/input（HID）或 /dev/bus/usb（原始 USB）。",
			"A device in Fabo is gone from the Mac until you detach it. In a container: --device /dev/ttyUSB0 (serial), /dev/input (HID) or /dev/bus/usb (raw).")
	}
	return tr("交给 Fabo 的设备在 Windows 里会消失，直到你把它还回去。在容器里使用：--device /dev/ttyUSB0（串口）、/dev/input（HID）或 /dev/bus/usb（原始 USB）。",
		"A device in Fabo is gone from Windows until you detach it. In a container: --device /dev/ttyUSB0 (serial), /dev/input (HID) or /dev/bus/usb (raw).")
}

// usbRow is one device: its name, ID, where it is, since when, and the
// button that moves it. A narrow table leaves out the ID and the time.
func (s *desktop) usbRow(rc *ui.Context, p palette, narrow bool, actionsWidth float32, d hostUSBDevice) {
	s.tableRow(rc, p, d.BusID, d.Name, actionsWidth, nil, func() {
		cell(rc, 0, 1).Children(func() { label(rc, d.Name, p.text).FontWeight(500) })
		if !narrow {
			cell(rc, 96, 0).Children(func() {
				if d.ID == "" {
					label(rc, "—", p.muted)
				} else {
					label(rc, d.ID, p.muted)
				}
			})
		}
		if !s.usbCanMove() {
			if !narrow {
				cell(rc, 200, 0).Children(func() { label(rc, d.Path, p.muted) })
			}
			return
		}
		cell(rc, 120, 0).Gap(8).Children(func() {
			if d.InFabo {
				dot(rc, p, "running")
				label(rc, tr("在 Fabo 中", "In Fabo"), stateColor(p, "running"))
			} else {
				dot(rc, p, "")
				label(rc, s.usbHere(), p.muted)
			}
		})
		if !narrow {
			cell(rc, 86, 0).Children(func() {
				at := int64(0)
				if d.InFabo {
					at = d.Since
				}
				label(rc, since(s.now, at), p.muted)
			})
		}
	}, func() {
		if s.usbCanMove() {
			s.usbButton(rc, p, d)
		}
	})
}

// usbButton is a device's Attach or Detach, or what it is waiting for.
func (s *desktop) usbButton(c *ui.Context, p palette, d hostUSBDevice) {
	if s.usbBusy(d.BusID) {
		ui.Row(c).Gap(6).AlignItems(ui.Center).Children(func() {
			ui.Spinner(c).Size(14, 14)
			word := tr("正在交给 Fabo…", "Attaching…")
			if d.InFabo {
				word = tr("正在还回…", "Detaching…")
			}
			ui.Text(c, word).FontSize(12).TextColor(p.muted).SingleLine()
		})
		return
	}
	if d.InFabo {
		back := tr("还给这台电脑：", "Give back to this PC: ")
		if s.usbOnMac() {
			back = tr("还给这台 Mac：", "Give back to the Mac: ")
		}
		if labeledButton(c.Key("detach"), p, tr("还回", "Detach"), back+d.Name, "", plainButton, false) {
			s.usbAct("detach", d.BusID, d.Name)
		}
		return
	}
	if labeledButton(c.Key("attach"), p, tr("交给 Fabo", "Attach"), tr("交给 Fabo 的容器：", "Hand to Fabo's containers: ")+d.Name, "", primaryButton, false) {
		s.usbAct("attach", d.BusID, d.Name)
	}
}

// usbDock is the selected device's details.
func (s *desktop) usbDock(c *ui.Context, p palette, d hostUSBDevice) {
	var fields []field
	switch {
	case !s.usbCanMove():
		// Linux: nothing to move, only where containers find it.
		fields = []field{{"ID", d.ID}, {tr("路径", "Path"), d.Path}, {tr("设备节点", "Device node"), d.BusID}}
	case s.host == nil:
		// A Mac: the location `fabo usb attach` takes, and the bus speed.
		where := s.usbHere()
		if d.InFabo {
			where = tr("在 Fabo 中", "In Fabo")
		}
		fields = []field{{tr("状态", "State"), where}, {"ID", d.ID}, {tr("位置", "Location"), d.BusID}, {tr("速度", "Speed"), d.Speed}}
	default:
		where := s.usbHere()
		if d.InFabo {
			where = tr("在 Fabo 中", "In Fabo")
		}
		shared := tr("否：第一次交给 Fabo 时 Windows 会请求管理员批准", "No: Windows asks an administrator the first time it goes to Fabo")
		if d.Shared {
			shared = tr("是", "Yes")
		}
		fields = []field{{tr("状态", "State"), where}, {"ID", d.ID}, {tr("序列号", "Serial number"), d.Serial},
			{tr("总线 ID", "Bus ID"), d.BusID}, {tr("已共享", "Shared"), shared}}
	}
	if d.InFabo && d.Since > 0 {
		fields = append(fields, field{tr("接入时间", "Attached"), strings.TrimPrefix(since(s.now, d.Since), "—")})
	}
	var actions func()
	if s.usbCanMove() {
		actions = func() { s.usbButton(c, p, d) }
	}
	s.dock(c, p, d.Name, nil, []string{tr("信息", "Info")}, actions, func(int) {
		keyValues(c, p, compact(fields))
	})
}

// usbEmpty is the page's centered state: a title, an optional line under
// it and an optional button, which reports whether it was clicked.
func usbEmpty(c *ui.Context, p palette, title, body, action string, busy bool) bool {
	clicked := false
	ui.Column(c).Fill().Grow(1).MinHeight(120).Center().Gap(12).Padding(0, 40).Children(func() {
		ui.Box(c).Size(52, 52).Radius(14).Background(p.soft).Center().Children(func() {
			ui.Icon(c, icons["usb"]).Size(24, 24).TextColor(p.muted)
		})
		ui.Text(c, title).FontSize(13.5).FontWeight(500).TextColor(p.text)
		if body != "" {
			ui.Text(c, body).FontSize(12).TextColor(p.muted).MaxWidth(460).TextAlign(ui.Center).Selectable()
		}
		if action != "" {
			clicked = button(c.Key("usb-action"), p, action, "", primaryButton, busy)
		}
	})
	return clicked
}
