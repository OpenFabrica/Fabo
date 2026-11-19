package guestagent

import (
	"context"
	"log"
	"time"

	"golang.org/x/sys/unix"

	"github.com/OpenFabrica/Fabo/engine/internal/guestwire"
)

// memoryPressureTrigger is the PSI trigger the agent arms: some task waited
// on memory for 50 ms of a 500 ms window. The kernel replaces the last byte
// written with a NUL, so the string carries its own.
const memoryPressureTrigger = "some 50000 500000\x00"

// watchMemoryPressure tells the host each time the guest stalls on memory,
// at most ten times a second, so the host can give the VM more memory at
// once instead of on its next once-a-second sample. It returns when ctx
// ends, the session write fails, or the kernel has no PSI.
func watchMemoryPressure(ctx context.Context, write func(guestwire.Frame) error) {
	fd, err := unix.Open("/proc/pressure/memory", unix.O_RDWR|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if err != nil {
		return
	}
	defer unix.Close(fd)
	if _, err := unix.Write(fd, []byte(memoryPressureTrigger)); err != nil {
		log.Printf("memory pressure trigger: %v", err)
		return
	}
	var last time.Time
	fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLPRI}}
	for ctx.Err() == nil {
		fds[0].Revents = 0
		// A timeout, so a closed session ends the loop within a second.
		n, err := unix.Poll(fds, 1000)
		if err == unix.EINTR || n == 0 {
			continue
		}
		if err != nil || fds[0].Revents&(unix.POLLERR|unix.POLLNVAL) != 0 {
			return
		}
		// The trigger is edge-like: poll reports it once per firing, and
		// the kernel fires at most once per window.
		if fds[0].Revents&unix.POLLPRI == 0 || time.Since(last) < 100*time.Millisecond {
			continue
		}
		last = time.Now()
		if write(guestwire.Frame{Kind: guestwire.MemoryPressure}) != nil {
			return
		}
	}
}
