package main

import (
	"encoding/json"
	"testing"

	"github.com/OpenFabrica/Fabo/engine/internal/usbhost"
)

// usbListGolden is `fabo usb list --json` for TestMacUSBRows; desktop's
// TestUSBOnMac decodes the same text, the contract between the two modules.
const usbListGolden = `{"can":"attach","devices":[{"busid":"0x01100000","name":"CP2102 USB to UART","id":"10c4:ea60","kind":"serial","speed":"full","inFabo":true,"since":1763370000},{"busid":"0x01200000","name":"Keyboard","id":"05ac:024f","speed":"low","inFabo":false},{"busid":"fake-serial","name":"Fabo fake serial","id":"","inFabo":true,"since":1763370100}]}`

// TestMacUSBRows joins the Mac's devices with the engine's sessions,
// keeping a session with no device on the bus (a fake device).
func TestMacUSBRows(t *testing.T) {
	devices := []usbhost.Device{
		{Location: "0x01100000", Name: "CP2102 USB to UART", Kind: "serial", Vendor: 0x10c4, Product: 0xea60, Speed: 1},
		{Location: "0x01200000", Name: "Keyboard", Vendor: 0x05ac, Product: 0x024f, Speed: 0},
	}
	sessions := []usbSession{{Location: "0x01100000", Name: "CP2102", Since: 1763370000}, {Location: "fake-serial", Name: "Fabo fake serial", Since: 1763370100}}
	raw, err := json.Marshal(usbList{Can: "attach", Devices: macUSBRows(devices, sessions)})
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != usbListGolden {
		t.Fatalf("got  %s\nwant %s", raw, usbListGolden)
	}
}
