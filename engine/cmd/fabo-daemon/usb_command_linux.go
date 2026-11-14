package main

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/OpenFabrica/FabriBay/engine/internal/usbhost"
	"os"
)

func usbHostCommand(_ context.Context, args []string) error {
	if len(args) == 1 && args[0] == "list" {
		d, e := usbhost.List()
		if e != nil {
			return e
		}
		return json.NewEncoder(os.Stdout).Encode(d)
	}
	return errors.New("USB capture is only needed on macOS; use --device on Linux and usbipd-win for WSL")
}
