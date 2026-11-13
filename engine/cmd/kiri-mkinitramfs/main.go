// kiri-mkinitramfs packs the guest's PID 1 into the initramfs the VM boots.
package main

import (
	"bytes"
	"fmt"
	"os"
)

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: kiri-mkinitramfs <init binary> <out.cpio>")
		os.Exit(2)
	}
	init, err := os.ReadFile(os.Args[1])
	if err == nil {
		err = os.WriteFile(os.Args[2], initramfs(init), 0o644)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "kiri-mkinitramfs:", err)
		os.Exit(1)
	}
}

// initramfs lays out the root the kernel unpacks: mount points, the console
// and /init. Uncompressed on purpose: it is small, and the kernel then skips
// decompression at every boot.
func initramfs(init []byte) []byte {
	var c cpio
	for _, dir := range []string{"dev", "proc", "sys", "run", "tmp", "etc"} {
		c.entry(dir, 0o040755, 2, 0, 0, nil)
	}
	// The kernel opens /dev/console for PID 1 before any filesystem is mounted.
	c.entry("dev/console", 0o020600, 1, 5, 1, nil)
	c.entry("init", 0o100755, 1, 0, 0, init)
	c.ino = 0
	c.entry("TRAILER!!!", 0, 1, 0, 0, nil)
	return c.buf.Bytes()
}

// cpio writes the "newc" archive format. Owner and mtime are fixed at zero,
// so the same init always packs to the same bytes.
type cpio struct {
	buf bytes.Buffer
	ino uint32
}

// entry appends one header, its NUL-terminated name and its data, each
// padded to four bytes as the format requires.
func (c *cpio) entry(name string, mode, nlink, rdevMajor, rdevMinor uint32, data []byte) {
	c.ino++
	c.buf.WriteString("070701")
	for _, field := range []uint32{
		c.ino, mode, 0 /* uid */, 0 /* gid */, nlink, 0 /* mtime */, uint32(len(data)),
		0 /* devmajor */, 0 /* devminor */, rdevMajor, rdevMinor, uint32(len(name) + 1), 0, /* check */
	} {
		fmt.Fprintf(&c.buf, "%08x", field)
	}
	c.buf.WriteString(name)
	c.buf.WriteByte(0)
	c.pad()
	c.buf.Write(data)
	c.pad()
}

// pad aligns the archive to the next four-byte boundary.
func (c *cpio) pad() {
	for c.buf.Len()%4 != 0 {
		c.buf.WriteByte(0)
	}
}
