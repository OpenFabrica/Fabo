package usbhost

import (
	"encoding/binary"
	"io"
	"net"
	"testing"
	"time"
)

func submit(seq uint32, ep uint32, in bool, data []byte, n int) []byte {
	h := make([]byte, 48)
	binary.BigEndian.PutUint32(h, 1)
	binary.BigEndian.PutUint32(h[4:], seq)
	binary.BigEndian.PutUint32(h[16:], ep)
	if in {
		binary.BigEndian.PutUint32(h[12:], 1)
	}
	binary.BigEndian.PutUint32(h[24:], uint32(n))
	return append(h, data...)
}
func unlink(seq, target uint32) []byte {
	h := make([]byte, 48)
	binary.BigEndian.PutUint32(h, 2)
	binary.BigEndian.PutUint32(h[4:], seq)
	binary.BigEndian.PutUint32(h[20:], target)
	return h
}
func readUSB(t *testing.T, c net.Conn) (uint32, uint32, int32, []byte) {
	t.Helper()
	c.SetReadDeadline(time.Now().Add(3 * time.Second))
	h := make([]byte, 48)
	if _, e := io.ReadFull(c, h); e != nil {
		t.Fatal(e)
	}
	command := binary.BigEndian.Uint32(h)
	var data []byte
	if command == 3 {
		data = make([]byte, binary.BigEndian.Uint32(h[24:]))
		if _, e := io.ReadFull(c, data); e != nil {
			t.Fatal(e)
		}
	}
	return command, binary.BigEndian.Uint32(h[4:]), int32(binary.BigEndian.Uint32(h[20:])), data
}
func TestUSBQueuedAndRunningCancellation(t *testing.T) {
	server, client := net.Pipe()
	f := NewFake(false, 0)
	done := make(chan error, 1)
	go func() { done <- Serve(f, server) }()
	defer client.Close()
	client.Write(submit(1, 1, true, nil, 8))
	client.Write(submit(2, 1, true, nil, 8))
	client.Write(unlink(3, 2))
	cmd, seq, status, _ := readUSB(t, client)
	if cmd != 4 || seq != 3 || status != -104 {
		t.Fatal(cmd, seq, status)
	}
	client.Write(unlink(4, 1))
	cmd, seq, status, _ = readUSB(t, client)
	if cmd != 4 || seq != 4 || status != -104 {
		t.Fatal(cmd, seq, status)
	}
	client.Write(unlink(5, 1))
	cmd, seq, status, _ = readUSB(t, client)
	if cmd != 4 || seq != 5 || status != 0 {
		t.Fatal(cmd, seq, status)
	}
	// Closing the stream must release another blocked notification read.
	client.Write(submit(6, 1, true, nil, 8))
	client.Close()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("disconnect left workers blocked")
	}
}
func TestUSBControlLengthAndEcho(t *testing.T) {
	server, client := net.Pipe()
	f := NewFake(false, 0)
	done := make(chan error, 1)
	go func() { done <- Serve(f, server) }()
	defer func() { client.Close(); <-done }()
	h := submit(1, 0, true, nil, 64)
	h[40] = 0x80
	h[41] = 6
	binary.LittleEndian.PutUint16(h[42:], 0x100)
	binary.LittleEndian.PutUint16(h[46:], 8)
	client.Write(h)
	_, _, status, data := readUSB(t, client)
	if status != 0 || len(data) != 8 || data[0] != 18 {
		t.Fatal(status, data)
	}
	// Complete OUT and IN concurrently; replies for OUT carry no payload.
	client.Write(submit(2, 2, true, nil, 3))
	client.Write(submit(3, 2, false, []byte("abc"), 3))
	for n := 0; n < 2; n++ {
		h = make([]byte, 48)
		client.SetReadDeadline(time.Now().Add(3 * time.Second))
		if _, e := io.ReadFull(client, h); e != nil {
			t.Fatal(e)
		}
		seq := binary.BigEndian.Uint32(h[4:])
		if binary.BigEndian.Uint32(h[24:]) != 3 {
			t.Fatal(h)
		}
		if seq == 2 {
			p := make([]byte, 3)
			io.ReadFull(client, p)
			if string(p) != "abc" {
				t.Fatal(string(p))
			}
		} else if seq != 3 {
			t.Fatal(seq)
		}
	}
}
func TestUSBRejectsInvalidHeader(t *testing.T) {
	for _, h := range [][]byte{submit(1, 16, true, nil, 0), submit(1, 1, true, nil, -1), submit(1, 1, true, nil, maxTransfer+1)} {
		server, client := net.Pipe()
		done := make(chan error, 1)
		go func() { done <- Serve(NewFake(false, 0), server) }()
		client.Write(h)
		if e := <-done; e == nil {
			t.Fatal("accepted invalid header")
		}
		client.Close()
	}
}
func TestUSBFindAmbiguous(t *testing.T) {
	devices := []Device{{Location: "0x02100000", Vendor: 0x1209, Product: 1, Name: "Test serial"}, {Location: "0x02200000", Vendor: 0x1209, Product: 1, Name: "Test serial"}}
	if _, e := Find(devices, "1209:0001"); e == nil {
		t.Fatal("ambiguous match")
	}
	if d, e := Find(devices, "2100000"); e != nil || d.Location != devices[0].Location {
		t.Fatal(d, e)
	}
}

func TestFakeStaysHaltedUntilClear(t *testing.T) {
	f := NewFake(false, 0)
	f.StallOnce = true
	f.Transfer(1, 2, []byte("payload"), 7)
	for _, seq := range []uint32{2, 3, 4} {
		if r := f.Transfer(seq, 0x82, nil, 7); r.Status != -32 || len(r.Data) != 0 {
			t.Fatal(r)
		}
	}
	f.ClearHalt(0x82)
	if r := f.Transfer(5, 0x82, nil, 7); r.Status != 0 || string(r.Data) != "payload" {
		t.Fatal(r)
	}
	// A late cancellation may refer to a completed transfer; it must not
	// cancel the next URB on the same endpoint.
	f.Abort(5, 0x82)
	f.Transfer(6, 2, []byte("next"), 4)
	if r := f.Transfer(7, 0x82, nil, 4); r.Status != 0 || string(r.Data) != "next" {
		t.Fatal(r)
	}
}
