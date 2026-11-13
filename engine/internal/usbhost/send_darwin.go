package usbhost

import (
	"bufio"
	"errors"
	"golang.org/x/sys/unix"
	"net"
	"strings"
	"time"
)

// SendToHelper transfers only an already-connected socket; the root process
// never opens a user-selected path. Capture errors are returned before success.
func SendToHelper(helper *net.UnixConn, stream *net.UnixConn, location string) error {
	file, e := stream.File()
	if e != nil {
		return e
	}
	defer file.Close()
	message := "attach " + location + "\n"
	n, _, e := helper.WriteMsgUnix([]byte(message), unix.UnixRights(int(file.Fd())), nil)
	if e != nil {
		return e
	}
	if n != len(message) {
		return errors.New("short USB helper request")
	}
	_ = helper.SetReadDeadline(time.Now().Add(30 * time.Second))
	line, e := bufio.NewReader(helper).ReadString('\n')
	if e != nil {
		return e
	}
	if strings.TrimSpace(line) != "ok" {
		return errors.New(strings.TrimPrefix(strings.TrimSpace(line), "error: "))
	}
	return nil
}
