package main

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"sync"

	"github.com/libracoder/kiri/engine/internal/guestwire"
)

func startEngineSocket() (func(), error) {
	path := guestwire.EngineSocketPath
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return nil, err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0660); err != nil {
		_ = listener.Close()
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	slots := make(chan struct{}, 128)
	var workers sync.WaitGroup
	workers.Go(func() {
		for {
			client, err := listener.AcceptUnix()
			if err != nil {
				return
			}
			select {
			case slots <- struct{}{}:
			default:
				_ = client.Close()
				continue
			}
			workers.Go(func() {
				defer func() { <-slots }()
				defer client.Close()
				host, err := dialHostRelay()
				if err != nil {
					return
				}
				defer host.Close()
				stop := context.AfterFunc(ctx, func() {
					_ = client.Close()
					_ = host.Close()
				})
				defer stop()
				if err := guestwire.WriteRelayTarget(host, guestwire.EngineRelayTarget); err == nil {
					guestwire.BridgeTCP(client, host)
				}
			})
		}
	})
	return func() {
		cancel()
		_ = listener.Close()
		workers.Wait()
	}, nil
}
