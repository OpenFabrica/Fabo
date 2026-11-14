//go:build linux

// fabo-host-run runs a local image using the Fabo agent on a Linux host.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/OpenFabrica/FabriBay/engine/internal/guestwire"
	"github.com/OpenFabrica/FabriBay/engine/internal/hostagent"
)

type launch struct {
	Run    guestwire.RunSpec
	Layers []hostagent.Layer
}

func main() {
	binary := flag.String("agent", "", "path to the Fabo agent")
	spec := flag.String("spec", "", "JSON launch specification")
	flag.Parse()
	if *binary == "" || *spec == "" {
		flag.Usage()
		os.Exit(2)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	code, err := run(ctx, *binary, *spec, os.Stdout, os.Stderr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "fabo-host-run:", err)
		os.Exit(1)
	}
	os.Exit(int(code))
}

func run(ctx context.Context, binary, path string, stdout, stderr io.Writer) (code int32, err error) {
	file, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	var spec launch
	err = json.NewDecoder(file).Decode(&spec)
	file.Close()
	if err != nil {
		return 0, err
	}
	if spec.Run.ID == "" || len(spec.Run.Argv) == 0 || len(spec.Run.Layers) == 0 {
		return 0, errors.New("run specification requires ID, Argv and Layers")
	}
	p, err := hostagent.Start(ctx, binary, stderr)
	if err != nil {
		return 0, err
	}
	defer func() { err = errors.Join(err, p.Close()) }()

	type outcome struct {
		code int32
		err  error
	}
	result := make(chan outcome, 1)
	var outputErr error
	s := guestwire.NewSession(p.Conn, func(event guestwire.RunEvent) {
		if event.ID != spec.Run.ID {
			return
		}
		switch event.Kind {
		case guestwire.OutputMessage:
			if outputErr == nil {
				w := stdout
				if event.Stream == 2 {
					w = stderr
				}
				_, outputErr = w.Write(event.Data)
			}
		case guestwire.ExitedMessage:
			result <- outcome{event.Exit.Code, outputErr}
		}
	})
	defer s.Close()
	stop := context.AfterFunc(ctx, func() { _ = s.Close() })
	defer stop()
	if err := p.Conn.SetDeadline(time.Now().Add(2 * time.Minute)); err != nil {
		return 0, err
	}
	if _, err := hostagent.StartImage(ctx, s, spec.Run, spec.Layers); err != nil {
		return 0, err
	}
	if err := p.Conn.SetDeadline(time.Time{}); err != nil {
		return 0, err
	}
	select {
	case r := <-result:
		return r.code, r.err
	case <-s.Done():
		select {
		case r := <-result:
			return r.code, r.err
		default:
			return 0, errors.Join(ctx.Err(), s.Err())
		}
	}
}
