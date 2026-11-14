//go:build linux || (darwin && cgo)

package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/OpenFabrica/FabriBay/engine/internal/guestwire"
	"github.com/OpenFabrica/FabriBay/engine/internal/hostagent"
	"github.com/OpenFabrica/FabriBay/engine/internal/localengine"
)

type rawOptions struct {
	image           string
	command, env    []string
	user, cwd       *string
	remove, timings bool
}

const rawUsage = "usage: fabo raw run [--rm] [-e KEY=VALUE] [-u USER] [-w DIR] [--timings] [--network none] IMAGE [COMMAND...]"

func parseRaw(args []string) (rawOptions, error) {
	var o rawOptions
	for len(args) > 0 {
		a := args[0]
		args = args[1:]
		switch a {
		case "--rm":
			o.remove = true
		case "--timings":
			o.timings = true
		case "-e", "--env", "-u", "--user", "-w", "--workdir", "--network", "--net":
			if len(args) == 0 {
				return o, errors.New(rawUsage)
			}
			v := args[0]
			args = args[1:]
			switch a {
			case "-e", "--env":
				if !strings.Contains(v, "=") {
					return o, errors.New("raw environment requires KEY=VALUE")
				}
				o.env = append(o.env, v)
			case "-u", "--user":
				o.user = &v
			case "-w", "--workdir":
				o.cwd = &v
			default:
				if v != "none" {
					return o, errors.New("raw run supports only --network none")
				}
			}
		default:
			if strings.HasPrefix(a, "-") {
				return o, errors.New(rawUsage)
			}
			o.image = a
			o.command = args
			return o, nil
		}
	}
	return o, errors.New(rawUsage)
}
func rawSpec(o rawOptions, image localengine.Image, id string) guestwire.RunSpec {
	cmd := image.Cmd
	if len(o.command) > 0 {
		cmd = o.command
	}
	argv := append(slices.Clone(image.Entrypoint), cmd...)
	env := slices.Clone(image.Env)
	for _, entry := range o.env {
		key, _, _ := strings.Cut(entry, "=")
		env = slices.DeleteFunc(env, func(s string) bool { return strings.HasPrefix(s, key+"=") })
		env = append(env, entry)
	}
	user, cwd := image.User, image.WorkingDir
	if o.user != nil {
		user = *o.user
	}
	if o.cwd != nil {
		cwd = *o.cwd
	}
	layers := []string{}
	for _, layer := range slices.Backward(image.Layers) {
		layers = append(layers, layer.Digest)
	}
	flags := guestwire.RunNetworkNone
	if o.remove {
		flags |= guestwire.RunAutoRemove
	}
	return guestwire.RunSpec{ID: id, Hostname: id, Argv: argv, Env: env, Cwd: cwd, User: user, Layers: layers, Flags: flags}
}

// Each invocation holds the store lock through guest shutdown. Concurrent raw
// runs fail before booting or touching the shared writable-layer data disk.
func rawMain(ctx context.Context, args []string) (code int, err error) {
	flags := flag.NewFlagSet("raw", flag.ContinueOnError)
	var guest guestOptions
	guest.register(flags)
	dir := flags.String("state", "", "raw image and guest state")
	if e := flags.Parse(args); e != nil {
		return 1, e
	}
	words := flags.Args()
	if *dir == "" || len(words) == 0 {
		return 1, errors.New("raw requires state and a command")
	}
	verb, rest := words[0], words[1:]
	var options rawOptions
	var e error
	runs := 0
	switch verb {
	case "run":
		options, e = parseRaw(rest)
	case "bench":
		if len(rest) != 2 {
			return 1, errors.New("raw bench requires RUNS IMAGE")
		}
		runs, e = strconv.Atoi(rest[0])
		if e == nil && runs < 1 {
			e = errors.New("runs must be positive")
		}
		options = rawOptions{image: rest[1], command: []string{"true"}, remove: true}
	case "pull":
		if len(rest) != 1 {
			return 1, errors.New("usage: fabo raw pull IMAGE")
		}
	case "images":
		if len(rest) != 0 {
			return 1, errors.New("usage: fabo raw images")
		}
	case "rmi":
		if len(rest) == 0 {
			return 1, errors.New("usage: fabo raw rmi IMAGE...")
		}
	default:
		return 1, errors.New("unknown raw command")
	}
	if e != nil {
		return 1, e
	}
	if e = os.MkdirAll(*dir, 0700); e != nil {
		return 1, e
	}
	state, e := localengine.OpenState(*dir)
	if e != nil {
		return 1, e
	}
	defer state.Close()
	store, e := localengine.OpenOfflineImages(ctx, state)
	if e != nil {
		return 1, e
	}
	switch verb {
	case "pull":
		start := time.Now()
		image, e := store.Pull(ctx, rest[0])
		if e != nil {
			return 1, e
		}
		fmt.Printf("%s %s — %d bytes in %.2f s\n", rest[0], shortRawDigest(image.ID), image.Size, time.Since(start).Seconds())
		return 0, nil
	case "images":
		fmt.Printf("%-45s %-12s %12s %7s\n", "IMAGE", "DIGEST", "SIZE", "LAYERS")
		for _, v := range store.List() {
			fmt.Printf("%-45s %-12s %12d %7d\n", v.Name, shortRawDigest(v.Image.ID), v.Image.Size, len(v.Image.Layers))
		}
		return 0, nil
	case "rmi":
		for _, ref := range rest {
			if e := store.Remove(ref); e != nil {
				return 1, e
			}
			fmt.Printf("removed %s\n", ref)
		}
		return 0, nil
	}
	image, e := store.Find(options.image)
	if e != nil {
		fmt.Fprintf(os.Stderr, "Unable to find image '%s' locally; pulling\n", options.image)
		image, e = store.Pull(ctx, options.image)
		if e != nil {
			return 1, e
		}
	}
	if len(rawSpec(options, image, "check").Argv) == 0 {
		return 1, errors.New("image has no default command; give one")
	}
	if !guest.valid() {
		return 1, errors.New("raw run requires the guest kernel/agent")
	}
	start := time.Now()
	if e = guest.prepare(ctx); e != nil {
		return 1, e
	}
	connection, e := guest.start(ctx, *dir)
	if e != nil {
		return 1, e
	}
	defer func() { err = errors.Join(err, connection.close()) }()
	booted := time.Since(start)
	ctx, cancelRun := context.WithCancel(ctx)
	events := make(chan guestwire.RunEvent, 64)
	session := guestwire.NewSession(connection.conn, func(event guestwire.RunEvent) {
		event.Data = slices.Clone(event.Data)
		select {
		case events <- event:
		case <-ctx.Done():
		}
	})
	defer session.Close()
	defer cancelRun()
	stop := context.AfterFunc(ctx, func() { _ = session.Close() })
	defer stop()
	spec := rawSpec(options, image, "layers")
	if e = hostagent.EnsureLayers(ctx, session, spec.Layers, image.Layers); e != nil {
		return 1, e
	}
	layerTime := time.Since(start) - booted
	count := 1
	if runs > 0 {
		count = runs + 5
	}
	result := rawBenchResult{}
	for n := 0; n < count; n++ {
		var id [6]byte
		if _, e = rand.Read(id[:]); e != nil {
			return 1, e
		}
		spec = rawSpec(options, image, hex.EncodeToString(id[:]))
		if runs > 0 {
			spec.Argv = []string{"true"}
			spec.User = ""
			spec.Cwd = "/"
		}
		began := time.Now()
		if _, e = session.Start(ctx, spec); e != nil {
			return 1, e
		}
		exit, e := rawWait(ctx, session, events, spec.ID, runs > 0)
		if e != nil {
			return 1, e
		}
		elapsed := time.Since(began)
		if runs > 0 {
			if exit.Code != 0 {
				return 1, fmt.Errorf("benchmark container exited %d", exit.Code)
			}
			if n >= 5 {
				result.Nanoseconds = append(result.Nanoseconds, int64(elapsed))
				result.SetupNS += exit.SetupNS
				result.RunNS += exit.RunNS
			}
			continue
		}
		if options.timings {
			fmt.Fprintf(os.Stderr, "fabo raw run timings\n  VM boot %.1f ms\n  layers ready %.1f ms\n  container start → exit %.2f ms (guest: setup %.2f ms, process %.2f ms)\n  total %.1f ms\n", float64(booted)/1e6, float64(layerTime)/1e6, float64(elapsed)/1e6, float64(exit.SetupNS)/1e6, float64(exit.RunNS)/1e6, float64(time.Since(start))/1e6)
		}
		return int(exit.Code), nil
	}
	return 0, json.NewEncoder(os.Stdout).Encode(result)
}

type rawBenchResult struct {
	Nanoseconds    []int64
	SetupNS, RunNS uint64
}

func shortRawDigest(id string) string {
	id = strings.TrimPrefix(id, "sha256:")
	return id[:min(len(id), 12)]
}
func rawWait(ctx context.Context, s *guestwire.Session, events <-chan guestwire.RunEvent, id string, quiet bool) (guestwire.Exit, error) {
	done := s.Done()
	for {
		var event guestwire.RunEvent
		select {
		case event = <-events:
		case <-ctx.Done():
			return guestwire.Exit{}, ctx.Err()
		case <-done:
			// Session closes Done only after its event callbacks return.
			// Drain every queued output frame before consuming the exit.
			select {
			case event = <-events:
			default:
				err := s.Err()
				if err == nil {
					err = io.ErrUnexpectedEOF
				}
				return guestwire.Exit{}, err
			}
		}
		if event.ID != id {
			continue
		}
		if event.Kind == guestwire.ExitedMessage {
			return event.Exit, nil
		}
		if event.Kind == guestwire.OutputMessage && !quiet {
			var out io.Writer = os.Stdout
			if event.Stream == 2 {
				out = os.Stderr
			}
			if _, err := out.Write(event.Data); err != nil {
				return guestwire.Exit{}, err
			}
		}
	}
}
