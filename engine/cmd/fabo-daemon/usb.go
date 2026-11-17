//go:build linux || (darwin && cgo)

package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"

	"github.com/OpenFabrica/Fabo/engine/internal/guestwire"
)

const usbPath = "/fabo/usb"

type usbDevice struct {
	location, name string
	speed, id      uint32
}

// usbStatus is one attached device as GET /fabo/usb lists it. Since is
// when it was attached (Unix seconds), for the window's Attached column,
// as upstream's daemon reports it.
type usbStatus struct {
	Location, Name string
	Port           int32
	Since          int64
}

type usbSession struct {
	usbStatus
	cancel context.CancelFunc
	done   chan struct{}
}

// usbSessions belongs to one VM. A pending attach reserves its location
// until both ends of the stream are closed.
type usbSessions struct {
	dial     guestwire.RelayDial
	mu       sync.Mutex
	closed   bool
	sessions map[string]*usbSession
}

func newUSBSessions(dial guestwire.RelayDial) *usbSessions {
	return &usbSessions{dial: dial, sessions: make(map[string]*usbSession)}
}

func parseUSBDevice(r *http.Request) (usbDevice, error) {
	q := r.URL.Query()
	location := q.Get("location")
	if len(location) == 0 || len(location) > 64 || strings.ContainsFunc(location, func(c rune) bool {
		return !(c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '-')
	}) {
		return usbDevice{}, fmt.Errorf("location must contain 1 to 64 letters, digits or dashes")
	}
	speed, err := strconv.ParseUint(q.Get("speed"), 10, 32)
	if err != nil || speed < 1 || speed > 6 {
		return usbDevice{}, fmt.Errorf("speed must be between 1 and 6")
	}
	var id uint64
	if raw := q.Get("devid"); raw != "" {
		id, err = strconv.ParseUint(raw, 10, 32)
		if err != nil {
			return usbDevice{}, fmt.Errorf("invalid USB device ID")
		}
	}
	upgrade := slices.ContainsFunc(strings.Split(strings.Join(r.Header.Values("Connection"), ","), ","), func(s string) bool {
		return strings.EqualFold(strings.TrimSpace(s), "upgrade")
	})
	if !upgrade || !strings.EqualFold(r.Header.Get("Upgrade"), "tcp") || r.ContentLength != 0 || len(r.TransferEncoding) != 0 {
		return usbDevice{}, fmt.Errorf("USB attachment requires a tcp upgrade without a request body")
	}
	name := make([]rune, 0, 80)
	for _, c := range q.Get("name") {
		if !unicode.IsControl(c) {
			name = append(name, c)
			if len(name) == cap(name) {
				break
			}
		}
	}
	return usbDevice{location, string(name), uint32(speed), uint32(id)}, nil
}

func (u *usbSessions) list(w http.ResponseWriter) {
	devices := make([]usbStatus, 0)
	if u != nil {
		u.mu.Lock()
		for _, session := range u.sessions {
			if session.Port >= 0 {
				devices = append(devices, session.usbStatus)
			}
		}
		u.mu.Unlock()
	}
	slices.SortFunc(devices, func(a, b usbStatus) int { return strings.Compare(a.Location, b.Location) })
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(devices)
}

func (u *usbSessions) detach(w http.ResponseWriter, r *http.Request, location string) {
	var session *usbSession
	if u != nil {
		u.mu.Lock()
		session = u.sessions[location]
		u.mu.Unlock()
	}
	if session == nil {
		supervisorError(w, http.StatusNotFound, fmt.Errorf("no USB device %s is attached", location))
		return
	}
	session.cancel()
	select {
	case <-session.done:
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"Location": location})
	case <-r.Context().Done():
	}
}

func (u *usbSessions) close() {
	if u == nil {
		return
	}
	u.mu.Lock()
	u.closed = true
	sessions := make([]*usbSession, 0, len(u.sessions))
	for _, session := range u.sessions {
		session.cancel()
		sessions = append(sessions, session)
	}
	u.mu.Unlock()
	for _, session := range sessions {
		<-session.done
	}
}

func (u *usbSessions) attach(w http.ResponseWriter, r *http.Request, device usbDevice) {
	if u == nil {
		supervisorError(w, http.StatusNotImplemented, fmt.Errorf("USB passthrough requires the Fabo VM on macOS"))
		return
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	session := &usbSession{usbStatus: usbStatus{Location: device.location, Name: device.name, Port: -1}, cancel: cancel, done: make(chan struct{})}
	u.mu.Lock()
	if u.closed {
		u.mu.Unlock()
		supervisorError(w, http.StatusServiceUnavailable, errManuallyStopped)
		return
	}
	if u.sessions[device.location] != nil {
		u.mu.Unlock()
		supervisorError(w, http.StatusConflict, fmt.Errorf("USB device %s is already attached", device.location))
		return
	}
	u.sessions[device.location] = session
	u.mu.Unlock()
	defer func() {
		u.mu.Lock()
		delete(u.sessions, device.location)
		close(session.done)
		u.mu.Unlock()
	}()

	guest, err := u.dial(ctx, guestwire.USBPort)
	if err != nil {
		supervisorError(w, http.StatusBadGateway, fmt.Errorf("connect to guest USB service: %w", err))
		return
	}
	defer guest.Close()
	stopGuest := context.AfterFunc(ctx, func() { _ = guest.Close() })
	defer stopGuest()
	port, err := requestUSBPort(guest, device)
	if err != nil {
		supervisorError(w, http.StatusBadGateway, fmt.Errorf("the VM could not attach the device: %w", err))
		return
	}
	if err := ctx.Err(); err != nil {
		supervisorError(w, http.StatusServiceUnavailable, err)
		return
	}
	helper, rw, err := http.NewResponseController(w).Hijack()
	if err != nil {
		supervisorError(w, http.StatusInternalServerError, err)
		return
	}
	defer helper.Close()
	stopHelper := context.AfterFunc(ctx, func() { _ = helper.Close() })
	defer stopHelper()
	if err := helper.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return
	}
	if _, err := rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n"); err != nil {
		return
	}
	if err := rw.Flush(); err != nil {
		return
	}
	if err := helper.SetDeadline(time.Time{}); err != nil {
		return
	}
	u.mu.Lock()
	session.Port, session.Since = port, time.Now().Unix()
	u.mu.Unlock()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = io.Copy(guest, rw.Reader)
		cancel()
	}()
	_, _ = io.Copy(helper, guest)
	cancel()
	<-done
}

func requestUSBPort(guest net.Conn, device usbDevice) (int32, error) {
	if err := guest.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return 0, err
	}
	var request [8]byte
	binary.LittleEndian.PutUint32(request[:4], device.speed)
	binary.LittleEndian.PutUint32(request[4:], device.id)
	if _, err := guest.Write(request[:]); err != nil {
		return 0, err
	}
	var reply [4]byte
	if _, err := io.ReadFull(guest, reply[:]); err != nil {
		return 0, err
	}
	port := int32(binary.LittleEndian.Uint32(reply[:]))
	if port < 0 {
		return 0, syscall.Errno(-int64(port))
	}
	return port, guest.SetDeadline(time.Time{})
}
