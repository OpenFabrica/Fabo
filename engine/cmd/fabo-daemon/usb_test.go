//go:build linux || (darwin && cgo)

package main

import (
	"context"
	"encoding/binary"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/OpenFabrica/Fabo/engine/internal/guestwire"
)

func TestUSBShutdownCancelsPendingAttachment(t *testing.T) {
	host, guest := net.Pipe()
	defer guest.Close()
	var dialed uint32
	sessions := newUSBSessions(func(_ context.Context, port uint32) (net.Conn, error) {
		dialed = port
		return host, nil
	})
	device := usbDevice{location: "fixture", speed: 2, id: 0x10002}
	request := httptest.NewRequest(http.MethodPost, usbPath, nil)
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		sessions.attach(httptest.NewRecorder(), request, device)
	}()
	_ = guest.SetReadDeadline(time.Now().Add(time.Second))
	var header [8]byte
	if _, err := io.ReadFull(guest, header[:]); err != nil {
		t.Fatal(err)
	}
	if dialed != guestwire.USBPort || binary.LittleEndian.Uint32(header[:4]) != device.speed ||
		binary.LittleEndian.Uint32(header[4:]) != device.id {
		t.Fatalf("invalid USB negotiation: port %d, header %x", dialed, header)
	}
	duplicate := httptest.NewRecorder()
	sessions.attach(duplicate, request, device)
	if duplicate.Code != http.StatusConflict {
		t.Fatalf("duplicate attachment returned %d: %s", duplicate.Code, duplicate.Body.String())
	}
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		sessions.close()
	}()
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("pending USB request held up shutdown")
	}
	<-finished
	list := httptest.NewRecorder()
	sessions.list(list)
	if list.Body.String() != "[]\n" {
		t.Fatalf("shutdown left an attached device: %s", list.Body.String())
	}
}

// TestUSBListSince: GET /fabo/usb lists attached devices with the time
// they were attached, and leaves out ones still connecting.
func TestUSBListSince(t *testing.T) {
	sessions := newUSBSessions(nil)
	sessions.sessions["0x01100000"] = &usbSession{usbStatus: usbStatus{Location: "0x01100000", Name: "CP2102", Port: 0, Since: 1763370000}}
	sessions.sessions["pending"] = &usbSession{usbStatus: usbStatus{Location: "pending", Port: -1}}
	list := httptest.NewRecorder()
	sessions.list(list)
	if got := list.Body.String(); got != `[{"Location":"0x01100000","Name":"CP2102","Port":0,"Since":1763370000}]`+"\n" {
		t.Fatalf("list = %s", got)
	}
}
