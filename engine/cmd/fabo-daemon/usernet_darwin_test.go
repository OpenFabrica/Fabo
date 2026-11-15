//go:build cgo

package main

import (
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/insomniacslk/dhcp/dhcpv4"
)

// TestUserNetworkLeasesTheGuestAddress plays the VM on the guest end of the
// socket pair: one Ethernet frame per datagram, a DHCP DISCOVER out, and an
// OFFER back with the fixed address, the gateway as router and as DNS.
func TestUserNetworkLeasesTheGuestAddress(t *testing.T) {
	mac := net.HardwareAddr{2, 1, 2, 3, 4, 5}
	n, err := startUserNetwork(context.Background(), mac.String())
	if err != nil {
		t.Fatal(err)
	}
	defer n.close()
	// Work on a duplicate, as the Virtualization framework does.
	fd, err := syscall.Dup(n.guestFD)
	if err != nil {
		t.Fatal(err)
	}
	file := os.NewFile(uintptr(fd), "guest")
	guest, err := net.FileConn(file)
	_ = file.Close()
	if err != nil {
		t.Fatal(err)
	}
	defer guest.Close()

	discover, err := dhcpv4.NewDiscovery(mac)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := guest.Write(dhcpFrame(mac, discover.ToBytes())); err != nil {
		t.Fatal(err)
	}
	_ = guest.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 2048)
	for {
		size, err := guest.Read(buf)
		if err != nil {
			t.Fatal(err)
		}
		offer, ok := dhcpReply(buf[:size])
		if !ok {
			continue // ARP and other frames the gateway sends on its own
		}
		if offer.MessageType() != dhcpv4.MessageTypeOffer || !offer.YourIPAddr.Equal(net.ParseIP(userNetGuest)) {
			t.Fatalf("offer: %s", offer.Summary())
		}
		gateway := net.ParseIP(userNetGateway)
		if routers := offer.Router(); len(routers) != 1 || !routers[0].Equal(gateway) {
			t.Fatalf("router %v", routers)
		}
		if dns := offer.DNS(); len(dns) != 1 || !dns[0].Equal(gateway) {
			t.Fatalf("dns %v", dns)
		}
		return
	}
}

// dhcpFrame wraps a DHCP payload in broadcast Ethernet, IPv4 and UDP
// headers (UDP checksum 0, which IPv4 allows).
func dhcpFrame(mac net.HardwareAddr, payload []byte) []byte {
	frame := make([]byte, 14+20+8+len(payload))
	copy(frame[0:6], net.HardwareAddr{0xff, 0xff, 0xff, 0xff, 0xff, 0xff})
	copy(frame[6:12], mac)
	binary.BigEndian.PutUint16(frame[12:14], 0x0800)
	ip := frame[14:34]
	ip[0], ip[8], ip[9] = 0x45, 64, 17
	binary.BigEndian.PutUint16(ip[2:4], uint16(20+8+len(payload)))
	copy(ip[16:20], net.IPv4bcast.To4())
	var sum uint32
	for i := 0; i < 20; i += 2 {
		sum += uint32(binary.BigEndian.Uint16(ip[i:]))
	}
	for sum > 0xffff {
		sum = sum&0xffff + sum>>16
	}
	binary.BigEndian.PutUint16(ip[10:12], ^uint16(sum))
	udp := frame[34:42]
	binary.BigEndian.PutUint16(udp[0:2], 68)
	binary.BigEndian.PutUint16(udp[2:4], 67)
	binary.BigEndian.PutUint16(udp[4:6], uint16(8+len(payload)))
	copy(frame[42:], payload)
	return frame
}

// dhcpReply returns the DHCP message in an IPv4/UDP frame from port 67.
func dhcpReply(frame []byte) (*dhcpv4.DHCPv4, bool) {
	if len(frame) < 14+20+8 || binary.BigEndian.Uint16(frame[12:14]) != 0x0800 || frame[23] != 17 {
		return nil, false
	}
	udp := 14 + int(frame[14]&0x0f)*4
	if len(frame) < udp+8 || binary.BigEndian.Uint16(frame[udp:]) != 67 {
		return nil, false
	}
	message, err := dhcpv4.FromBytes(frame[udp+8:])
	return message, err == nil
}

// TestUserNetworkCloseReleasesEverything starts and closes the network a few
// times, as sleeping and waking the VM does, and expects the goroutine count
// to come back: the stack, DHCP and DNS must not outlive a boot.
func TestUserNetworkCloseReleasesEverything(t *testing.T) {
	settle := func(want int) int {
		got := runtime.NumGoroutine()
		for deadline := time.Now().Add(3 * time.Second); got > want && time.Now().Before(deadline); got = runtime.NumGoroutine() {
			time.Sleep(20 * time.Millisecond)
		}
		return got
	}
	// One round first, so lazily started runtime helpers are in the baseline.
	n, err := startUserNetwork(context.Background(), "02:01:02:03:04:05")
	if err != nil {
		t.Fatal(err)
	}
	n.close()
	time.Sleep(100 * time.Millisecond)
	base := runtime.NumGoroutine()
	for i := range 3 {
		n, err := startUserNetwork(context.Background(), "02:01:02:03:04:05")
		if err != nil {
			t.Fatal(err)
		}
		n.close()
		if got := settle(base); got > base {
			t.Fatalf("round %d: %d goroutines, want %d", i, got, base)
		}
	}
}

// TestUserNetworkClosesWhileTheGuestStopsReading fills the guest's receive
// buffer, as a VM that has stopped would leave it, so the switch blocks in a
// write; close must still return.
func TestUserNetworkClosesWhileTheGuestStopsReading(t *testing.T) {
	mac := net.HardwareAddr{2, 1, 2, 3, 4, 5}
	n, err := startUserNetwork(context.Background(), mac.String())
	if err != nil {
		t.Fatal(err)
	}
	// A frame to the guest's own MAC: the switch sends it back to the guest.
	frame := make([]byte, 1500)
	copy(frame[0:6], mac)
	copy(frame[6:12], mac)
	if err := syscall.SetNonblock(n.guestFD, true); err != nil {
		t.Fatal(err)
	}
	// Keep writing until the switch stops draining our frames: it is then
	// stuck writing them back to a guest buffer nobody reads.
	for failures := 0; failures < 200; {
		if _, err := syscall.Write(n.guestFD, frame); err != nil {
			failures++
			time.Sleep(time.Millisecond)
		} else {
			failures = 0
		}
	}
	done := make(chan struct{})
	go func() {
		n.close()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("close blocked on a guest that no longer reads")
	}
}

// TestUserNetworkStartFailureCleansUp makes the socket pair fail after the
// stack, DHCP and DNS are up (no file descriptors left): start must return
// the error, not panic, and leave nothing running. It runs in a child
// process, since exhausting descriptors would disturb the test runner.
func TestUserNetworkStartFailureCleansUp(t *testing.T) {
	if os.Getenv("FABO_USERNET_EMFILE") == "1" {
		userNetworkStartFailureChild()
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestUserNetworkStartFailureCleansUp$", "-test.v")
	cmd.Env = append(os.Environ(), "FABO_USERNET_EMFILE=1")
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "start failed cleanly") {
		t.Fatalf("child: %v\n%s", err, out)
	}
}

// userNetworkStartFailureChild exhausts descriptors, starts the network and
// reports on stdout; a panic fails the parent through the exit status.
func userNetworkStartFailureChild() {
	time.Sleep(100 * time.Millisecond)
	base := runtime.NumGoroutine()
	var limit syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &limit); err != nil {
		panic(err)
	}
	low := limit
	low.Cur = 256
	if err := syscall.Setrlimit(syscall.RLIMIT_NOFILE, &low); err != nil {
		panic(err)
	}
	var held []int
	for {
		fd, err := syscall.Dup(0)
		if err != nil {
			break
		}
		held = append(held, fd)
	}
	n, err := startUserNetwork(context.Background(), "02:01:02:03:04:05")
	for _, fd := range held {
		_ = syscall.Close(fd)
	}
	if n != nil || err == nil {
		panic("start succeeded without file descriptors")
	}
	for deadline := time.Now().Add(3 * time.Second); runtime.NumGoroutine() > base && time.Now().Before(deadline); {
		time.Sleep(20 * time.Millisecond)
	}
	if got := runtime.NumGoroutine(); got > base {
		panic(fmt.Sprintf("%d goroutines left, want %d", got, base))
	}
	fmt.Println("start failed cleanly:", err)
}
