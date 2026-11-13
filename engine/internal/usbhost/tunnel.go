package usbhost

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// Connect returns a USB/IP socket ready to pass to the privileged helper.
// Read headers without prefetch: the VM may send its first URB immediately,
// and buffered bytes would otherwise be lost when transferring the descriptor.
func Connect(socket string, d Device) (*net.UnixConn, error) {
	conn, e := net.DialUnix("unix", nil, &net.UnixAddr{Name: socket, Net: "unix"})
	if e != nil {
		return nil, e
	}
	fail := func(e error) (*net.UnixConn, error) { conn.Close(); return nil, e }
	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
	q := url.Values{"location": {d.Location}, "name": {d.Name}, "speed": {strconv.Itoa(int(d.LinuxSpeed()))}, "devid": {strconv.Itoa(int(d.DeviceID()))}}
	req, e := http.NewRequest("POST", "http://kiri/kiri/usb?"+q.Encode(), nil)
	if e != nil {
		return fail(e)
	}
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "tcp")
	if e = req.Write(conn); e != nil {
		return fail(e)
	}
	header := []byte{}
	var one [1]byte
	for !bytes.HasSuffix(header, []byte("\r\n\r\n")) {
		if len(header) >= 64<<10 {
			return fail(errors.New("USB response headers too large"))
		}
		if _, e = io.ReadFull(conn, one[:]); e != nil {
			return fail(e)
		}
		header = append(header, one[0])
	}
	resp, e := http.ReadResponse(bufio.NewReader(io.MultiReader(bytes.NewReader(header), conn)), req)
	if e != nil {
		return fail(e)
	}
	if resp.StatusCode != 101 {
		defer resp.Body.Close()
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		var body struct{ Message string }
		_ = json.Unmarshal(raw, &body)
		if body.Message == "" {
			body.Message = string(raw)
		}
		return fail(fmt.Errorf("Fabo did not take the device: %s %s", resp.Status, body.Message))
	}
	_ = conn.SetDeadline(time.Time{})
	return conn, nil
}
