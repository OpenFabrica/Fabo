//go:build linux || (darwin && cgo)

// fabo-daemon serves the Docker API for the Go engine.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"
	"time"

	"github.com/OpenFabrica/Fabo/engine/internal/guestwire"
	"github.com/OpenFabrica/Fabo/engine/internal/localengine"
)

// The app build stamps its release version; standalone builds identify
// themselves with the development version.
var engineRelease = "fabo-go"

type guestConnection struct {
	conn    net.Conn
	close   func() error
	shares  localengine.ShareRoots
	disk    bool
	cgroups bool
	rosetta bool
	relay   guestwire.RelayDial
	archive guestwire.RelayDial
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "usb" {
		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
		defer cancel()
		if err := usbHostCommand(ctx, os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "fabo usb:", err)
			os.Exit(1)
		}
		return
	}

	if len(os.Args) > 1 && os.Args[1] == "raw" {
		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		code, err := rawMain(ctx, os.Args[2:])
		cancel()
		if err != nil {
			fmt.Fprintln(os.Stderr, "fabo raw:", err)
			os.Exit(1)
		}
		os.Exit(code)
	}

	if len(os.Args) > 1 && os.Args[1] == "migrate" {
		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer cancel()
		if err := migrateKiriCommand(ctx, os.Args[2:]); err != nil {
			log.Fatal(err)
		}
		return
	}
	var guest guestOptions
	guest.register(flag.CommandLine)
	socket := flag.String("socket", "", "daemon socket in an existing private directory")
	catalog := flag.String("images", "", "optional JSON catalog of local images")
	builder := flag.String("builder-container", "", "managed Buildx container name")
	stateDir := flag.String("state", "", "existing private directory for container records")
	tlsDir := flag.String("domain-tls-dir", "", "directory for the local domain authority")
	domainDefaults := flag.Bool("domains-default", false, "enable the standard domain listener ports")
	shareConfig := flag.String("share-config", "", "account file for public share links (share.json); empty disables them")
	flag.Parse()
	if !guest.valid() || *socket == "" || *stateDir == "" {
		flag.Usage()
		os.Exit(2)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := run(ctx, guest, *socket, *catalog, *stateDir, *builder, *tlsDir, *shareConfig, *domainDefaults); err != nil {
		log.Fatal(err)
	}
}

func run(ctx context.Context, guest guestOptions, socket, catalog, stateDir, builder, tlsDir, shareConfig string, domainDefaults bool) (err error) {
	socket, err = filepath.Abs(socket)
	if err != nil {
		return err
	}
	info, err := os.Stat(filepath.Dir(socket))
	if err != nil {
		return err
	}
	owner, ok := info.Sys().(*syscall.Stat_t)
	group := socketAccessGroup()
	forbidden := os.FileMode(0077)
	if group != "" {
		forbidden = 0022
	}
	if !info.IsDir() || info.Mode().Perm()&forbidden != 0 || !ok || owner.Uid != uint32(os.Geteuid()) {
		return fmt.Errorf("socket directory must be owned by the daemon user and have safe permissions")
	}
	var images map[string]localengine.Image
	if catalog != "" {
		raw, err := os.ReadFile(catalog)
		if err != nil {
			return err
		}
		if err := json.Unmarshal(raw, &images); err != nil {
			return err
		}
	}
	for ref, image := range images {
		if len(image.Layers) == 0 {
			return fmt.Errorf("image %q has no local layers", ref)
		}
	}
	state, err := localengine.OpenState(stateDir)
	if err != nil {
		return err
	}
	defer state.Close()

	socketListener, err := listenSocket(socket)
	if err != nil {
		return err
	}
	defer socketListener.Close()
	if err := os.Chmod(socket, 0600); err != nil {
		return err
	}
	listener, err := configureSocket(ctx, socketListener, group)
	if err != nil {
		return err
	}
	defer listener.Close()

	if err := guest.prepare(ctx); err != nil {
		return err
	}
	engineCtx := ctx
	if runtime.GOOS == "linux" {
		// Keep the agent's session alive while SIGTERM stops the workloads.
		var cancelEngine context.CancelFunc
		engineCtx, cancelEngine = context.WithCancel(context.WithoutCancel(ctx))
		defer cancelEngine()
	}
	windowsProxy := guest.configureEngineSocket(socket)
	manager := &supervisor{windowsProxy: windowsProxy, socket: socket, ctx: engineCtx, options: guest, state: state, stateDir: stateDir, images: images, builder: builder}
	manager.domains, err = localengine.OpenDomains(ctx, stateDir, tlsDir, domainDefaults, manager.dialDomain)
	if err != nil {
		return err
	}
	defer manager.domains.Close()
	if shareConfig != "" {
		manager.shares, err = localengine.OpenPublicShares(localengine.PublicShareOptions{
			ConfigPath: shareConfig, Version: engineRelease,
			ResolveContainer: manager.resolveShareContainer, DialContainer: manager.dialShare,
		})
		if err != nil {
			return fmt.Errorf("public share links: %w", err)
		}
		defer manager.shares.Close()
	}
	mirror, mirrorErr := localengine.OpenBuildMirror(engineCtx, manager.mirrorEngine)
	if mirrorErr != nil {
		log.Printf("local build images: %v", mirrorErr)
	} else {
		manager.buildMirror = mirror
		defer mirror.Close()
	}
	if err := manager.init(); err != nil {
		return err
	}
	defer func() { err = errors.Join(err, manager.close(runtime.GOOS == "linux" && ctx.Err() != nil)) }()
	stopFinder := guest.startFinder(manager)
	defer stopFinder()
	srv := &http.Server{Handler: manager, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second}
	monitor, cancel := context.WithCancel(ctx)
	defer cancel()
	idle := idleTimeout(os.Getenv("FABO_IDLE_SECS"), time.Minute)
	idleDone := make(chan struct{})
	go func() {
		defer close(idleDone)
		manager.monitorIdle(monitor, idle)
	}()
	defer func() {
		cancel()
		<-idleDone
	}()
	var guestDone <-chan struct{}
	first := manager.current.engine
	if manager.vm == nil {
		guestDone = first.Done()
	}
	go func() {
		select {
		case <-monitor.Done():
		case <-guestDone:
		}
		_ = srv.Close()
	}()
	if err := srv.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	if ctx.Err() == nil && manager.vm == nil {
		return first.GuestError()
	}
	return nil
}
