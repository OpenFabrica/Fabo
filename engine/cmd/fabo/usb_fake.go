//go:build linux || darwin

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/OpenFabrica/FabriBay/engine/internal/usbhost"
)

func usbFake(args []string) error {
	if len(args) == 0 || (args[0] != "serial" && args[0] != "mouse") {
		return errors.New("usage: fabo usb fake serial|mouse [--location L] [--unplug-after BYTES] [--stall-once] [--reports N]")
	}
	flags := flag.NewFlagSet("usb fake", flag.ContinueOnError)
	location := flags.String("location", fmt.Sprintf("fake-%d", os.Getpid()), "test device location")
	unplug := flags.Int("unplug-after", 0, "disconnect after bytes written")
	stall := flags.Bool("stall-once", false, "stall the first bulk read")
	reports := flags.Int("reports", 200, "mouse reports")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 || *reports < 0 || *unplug < 0 {
		return errors.New("invalid fake device options")
	}
	if *location == "" || strings.ContainsAny(*location, "/ ") || strings.Trim(*location, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-") != "" {
		return errors.New("--location takes letters, digits and dashes")
	}
	in, err := locate()
	if err != nil {
		return err
	}
	name := "Fabo test " + args[0]
	conn, err := usbhost.Connect(in.socket(), usbhost.Device{Location: *location, Name: name, Speed: 1, Address: 0x7f0000 | uint32(os.Getpid()&0x7f)})
	if err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	fake := usbhost.NewFake(args[0] == "mouse", *reports)
	fake.UnplugAfter = *unplug
	fake.StallOnce = *stall
	fake.Conn = conn
	fmt.Fprintf(os.Stderr, "fabo usb: %s attached as %s\n", name, *location)
	err = usbhost.Serve(fake, conn)
	echoed, cleared := fake.Stats()
	fmt.Fprintf(os.Stderr, "fabo usb: %s detached (%d bytes echoed, %d halts cleared)\n", name, echoed, cleared)
	if ctx.Err() != nil {
		return nil
	}
	return err
}
