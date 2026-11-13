package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/libracoder/kiri/engine/internal/guestagent"
	"github.com/libracoder/kiri/engine/internal/guestwire"
)

func startArchiveService(r *guestagent.Runtime, vm bool) (func(), error) {
	ctx, cancel := context.WithCancel(context.Background())
	var listener io.Closer
	slots := make(chan struct{}, 16)
	var wg sync.WaitGroup
	if vm {
		l, err := listenRelay(guestwire.CopyPort)
		if err != nil {
			cancel()
			return nil, err
		}
		listener = l
		wg.Go(func() {
			serveVSock(ctx, l, slots, &wg, func(ctx context.Context, conn *os.File) {
				_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
				r.ServeCopy(ctx, conn, func() (*os.File, error) {
					_ = conn.SetReadDeadline(time.Time{})
					return conn, nil
				})
			})
		})
	} else {
		path := filepath.Join(os.Getenv("KIRI_AGENT_DIR"), fmt.Sprintf("g%d.sock", guestwire.CopyPort))
		l, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
		if err != nil {
			cancel()
			return nil, err
		}
		listener = l
		wg.Go(func() {
			for {
				conn, err := l.AcceptUnix()
				if err != nil {
					return
				}
				select {
				case slots <- struct{}{}:
				default:
					conn.Close()
					continue
				}
				wg.Go(func() {
					defer func() { <-slots }()
					defer conn.Close()
					stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
					defer stop()
					_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
					r.ServeCopy(ctx, conn, func() (*os.File, error) {
						_ = conn.SetReadDeadline(time.Time{})
						return conn.File()
					})
				})
			}
		})
	}
	return func() {
		cancel()
		_ = listener.Close()
		wg.Wait()
	}, nil
}
