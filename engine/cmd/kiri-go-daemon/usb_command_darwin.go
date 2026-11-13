//go:build cgo

package main

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/libracoder/kiri/engine/internal/usbhost"
	"os"
	"strconv"
)

func usbHostCommand(ctx context.Context, args []string) error {
	if len(args) == 1 && args[0] == "list" {
		devices, e := usbhost.List()
		if e != nil {
			return e
		}
		return json.NewEncoder(os.Stdout).Encode(devices)
	}
	if len(args) == 3 && args[0] == "helper" {
		uid, e := strconv.ParseUint(args[2], 10, 32)
		if e != nil {
			return e
		}
		return usbhost.Helper(ctx, args[1], uint32(uid))
	}
	if len(args) == 6 && args[0] == "serve" && args[2] == "--fd" && args[4] == "--status" {
		fd, e := strconv.Atoi(args[3])
		if e != nil {
			return e
		}
		status, e := strconv.Atoi(args[5])
		if e != nil {
			return e
		}
		return usbhost.ServeCaptured(ctx, args[1], fd, status)
	}
	return errors.New("invalid USB host command")
}
