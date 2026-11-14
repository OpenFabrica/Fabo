//go:build linux || (darwin && cgo)

package main

import (
	"context"
	"net"
	"reflect"
	"testing"

	"github.com/OpenFabrica/Fabo/engine/internal/guestwire"
	"github.com/OpenFabrica/Fabo/engine/internal/hostagent"
	"github.com/OpenFabrica/Fabo/engine/internal/localengine"
)

func TestRawSpecOverridesAndLayerOrder(t *testing.T) {
	image := localengine.Image{Entrypoint: []string{"/entry"}, Cmd: []string{"default"}, Env: []string{"A=old", "B=keep"}, User: "root", WorkingDir: "/original", Layers: []hostagent.Layer{{Digest: "base"}, {Digest: "top"}}}
	options, e := parseRaw([]string{"--rm", "--timings", "-e", "A=new", "-w", "/tmp", "-u", "1234", "--network", "none", "alpine", "echo", "ok"})
	if e != nil {
		t.Fatal(e)
	}
	spec := rawSpec(options, image, "test")
	if !reflect.DeepEqual(spec.Argv, []string{"/entry", "echo", "ok"}) || !reflect.DeepEqual(spec.Env, []string{"B=keep", "A=new"}) || !reflect.DeepEqual(spec.Layers, []string{"top", "base"}) || spec.User != "1234" || spec.Cwd != "/tmp" || spec.Flags != guestwire.RunNetworkNone|guestwire.RunAutoRemove {
		t.Fatalf("%+v", spec)
	}
	if image.Env[0] != "A=old" || image.Entrypoint[0] != "/entry" {
		t.Fatal("mutated image")
	}
	for _, args := range [][]string{nil, {"--network", "bridge", "alpine"}, {"-e", "A", "alpine"}, {"-w"}, {"--bad", "alpine"}} {
		if _, e := parseRaw(args); e == nil {
			t.Fatal(args)
		}
	}
}
func TestRawWaitDrainsExitAfterDisconnect(t *testing.T) {
	for n := 0; n < 50; n++ {
		conn, peer := net.Pipe()
		s := guestwire.NewSession(conn, nil)
		peer.Close()
		<-s.Done()
		events := make(chan guestwire.RunEvent, 4)
		events <- guestwire.RunEvent{ID: "test", Kind: guestwire.OutputMessage, Data: []byte("one")}
		events <- guestwire.RunEvent{ID: "other", Kind: guestwire.ExitedMessage}
		events <- guestwire.RunEvent{ID: "test", Kind: guestwire.OutputMessage, Data: []byte("two")}
		events <- guestwire.RunEvent{ID: "test", Kind: guestwire.ExitedMessage, Exit: guestwire.Exit{Code: 37}}
		exit, e := rawWait(context.Background(), s, events, "test", true)
		if e != nil || exit.Code != 37 {
			t.Fatal(exit, e)
		}
		if _, e := rawWait(context.Background(), s, events, "test", true); e == nil {
			t.Fatal("disconnect without exit reported success")
		}
		s.Close()
	}
}
