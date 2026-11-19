//go:build linux || darwin

// Package localengine implements the Linux host's local-image container
// lifecycle over a Fabo guest control session.
package localengine

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"maps"
	"net"
	"os"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/OpenFabrica/Fabo/engine/internal/guestwire"
	"github.com/OpenFabrica/Fabo/engine/internal/hostagent"
)

type Image struct {
	Architecture, OS, Variant    string              `json:",omitempty"`
	ExposedPorts                 map[string]struct{} `json:",omitempty"`
	Created                      int64
	ID, RepoDigest               string
	Labels                       map[string]string
	DiffIDs                      []string
	ConfigBlob                   []byte `json:",omitempty"`
	Size                         int64
	Layers                       []hostagent.Layer
	Entrypoint, Cmd, Env         []string
	WorkingDir, User, StopSignal string
}

type Config struct {
	Labels                     map[string]string
	ExposedPorts               map[string]struct{}
	Volumes                    map[string]struct{}
	portBindings               []portBinding
	publishAll                 bool
	StopSignal                 string
	StopTimeout                *int64
	Healthcheck                *healthConfig
	Shell                      []string
	Image                      string
	platform                   string
	Entrypoint, Cmd, Env       []string
	WorkingDir, User, Hostname string
	Domainname                 string
	autoRemove                 bool
	privileged                 bool
	securityOpt                []string
	init                       *bool
	openStdin                  bool
	Tty                        bool
	consoleSize                [2]uint16
	logConfig                  logConfig
	bridge                     bool
	network                    string
	// pidMode names the container whose PID namespace this one joins.
	pidMode                    string
	networkEndpoints           map[string]endpointConfig
	networkMAC                 net.HardwareAddr
	links                      []string
	extraHosts                 []string
	dns                        guestwire.DNSConfig
	capabilities               capabilityConfig
	process                    guestwire.ProcessOptions
	devices                    guestwire.DeviceOptions
	sysctls                    map[string]string
	limits                     guestwire.Limits
	restart                    restartPolicy
	mounts                     []bindMount
	tmpfs                      []guestwire.TmpfsMount
	readOnlyRoot               bool
	volumes                    []volumeMount
}

type Engine struct {
	mu                sync.Mutex
	engineSocket      string
	windowsMounts     bool
	events            EventBus
	sharedEvents      *EventBus
	ctx               context.Context
	session           *guestwire.Session
	images            map[string]Image
	catalog           map[string]Image
	imageIndex        imageIndex
	imageOps          sync.Mutex
	builderOps        sync.Mutex
	builderUsers      int
	builds            map[string]context.CancelFunc
	pulls             sync.RWMutex
	containers        map[string]*container
	networks          map[string]dockerNetwork
	bridgeIPv6        *ipv6Pool
	bridgeIPv6Enabled bool
	networkOps        sync.Mutex
	execs             map[string]*execProcess
	state             *State
	proxy             *Proxy
	settled           chan struct{}
	closed            bool
	remoteGuest       bool
	rosetta           bool
	hostCgroups       bool
	guestShares       ShareRoots
	guestDisk         bool
	guestRelay        guestwire.RelayDial
	domains           *Domains
	guestArchive      guestwire.RelayDial
	release           string
	builderName       string
	buildMirror       *BuildMirror
	vmCPUs            uint
	vmMemory          uint64
	volumeRemovals    map[string]bool
	volumeCleanups    int
}

type container struct {
	labels                                   map[string]string
	exposedPorts                             map[string]struct{}
	portBindings                             []portBinding
	stopSignal                               uint32
	consoleSize                              [2]uint16
	stopTimeout                              int64
	id, name, image, imageID                 string
	networkName, networkID, networkInterface string
	extraNetworks                            []containerEndpoint
	links                                    []containerLink
	created                                  int64
	startedAt, finishedAt                    time.Time
	vmStopped                                bool
	builderUsedAt                            time.Time
	entrypoint                               []string
	capabilities                             capabilityConfig
	securityOpt                              []string
	run                                      guestwire.RunSpec
	init                                     *bool
	blobs                                    []hostagent.Layer
	state                                    string
	paused                                   bool
	healthcheck                              *healthConfig
	shell                                    []string
	health                                   *containerHealth
	restartPolicy                            restartPolicy
	restartCount                             int
	manualStopped                            bool
	restartTimer                             *time.Timer
	restartDelay                             time.Duration
	attempt                                  *runAttempt
	operation                                sync.Mutex
	probeStarting                            chan struct{}
	exit                                     int32
	failure                                  error
	logs                                     []record
	logBytes                                 int
	logConfig                                logConfig
	listeners                                map[*outputListener]struct{}
	removed                                  chan struct{}
	removing                                 bool
	oomKilled                                bool
	volumeNames                              []string
	anonymousVolumes                         []string
}

type runAttempt struct {
	done             chan struct{}
	started          chan struct{}
	// settled closes when the guest reports that this run has entered its
	// own root and namespaces (guestwire.SettledMessage), which can be after
	// Started. Exec and network joins wait for it, holding no lock, before
	// asking the guest. Nil when the run was not started through the guest.
	settled          chan struct{}
	startedAt        time.Time
	pid              uint32
	inputCancel      context.CancelFunc
	pauseDone        chan struct{}
	healthChanged    chan struct{}
	ports            *publishedPorts
	kubernetesPorts  map[kubernetesPort]*publishedPorts
	kubernetesShares map[uint16]*publishedPorts
	proxyNetwork     containerProxyNetwork
	joinedEnv        []string
	code             int32
	failure          error
}

// runtimeEnv is called under the engine mutex. A joiner's injected values
// are scoped to its live namespace and are not saved as user configuration.
func (c *container) runtimeEnv() []string {
	if c.attempt != nil && c.attempt.joinedEnv != nil {
		return c.attempt.joinedEnv
	}
	return c.run.Env
}

// nextAttempt is called with e.mu held.
func (c *container) nextAttempt() *runAttempt {
	if c.attempt == nil {
		c.attempt = &runAttempt{done: make(chan struct{}), started: make(chan struct{})}
	}
	return c.attempt
}

// markSettled records the guest's Settled event for this run, once. The
// caller holds the engine mutex.
func (a *runAttempt) markSettled() {
	if a.settled == nil {
		return
	}
	select {
	case <-a.settled:
	default:
		close(a.settled)
	}
}

// waitSettled waits until the run has entered its own root and namespaces,
// or has ended, which later checks report. The caller holds neither the
// engine mutex nor this run's container operation lock: a setup may take
// long and anything in the container may stall it, and pausing, signals and
// removal of that container must stay possible meanwhile. (A container
// joining its network waits holding its own operation lock, not the
// owner's.) The channels are fixed when the run starts, so reading them
// needs no lock.
func (a *runAttempt) waitSettled(ctx context.Context) error {
	if a == nil || a.settled == nil {
		return nil
	}
	select {
	case <-a.settled:
	case <-a.done:
	case <-ctx.Done():
		return ctx.Err()
	}
	return nil
}

func (a *runAttempt) close() {
	if a.ports != nil {
		a.ports.close()
	}
	for _, forward := range a.kubernetesPorts {
		forward.close()
	}
	for _, forward := range a.kubernetesShares {
		forward.close()
	}
	if a.inputCancel != nil {
		a.inputCancel()
	}
	close(a.done)
}

func (e *Engine) markStarted(c *container, pid uint32) {
	c.state = "running"
	c.startedAt = time.Now()
	c.builderUsedAt = c.startedAt
	c.attempt.startedAt = c.startedAt
	c.attempt.pid = pid
	// A --rm container's record is deleted when it exits and it has no
	// restart policy to replay after a crash, so its running state need not
	// reach the disk, as with Kiri.
	if c.run.Flags&guestwire.RunAutoRemove == 0 {
		if err := e.save(c); err != nil {
			// The guest has already started the process. Keep tracking the
			// live run and retry persistence when its terminal state is
			// recorded.
			log.Print(err)
		}
	}
	if c.attempt.ports != nil {
		c.attempt.ports.start()
	}
	e.startHealth(c)
	e.startKubernetes(c)
	e.emitContainer(c, "start", nil)
	close(c.attempt.started)
}

type record struct {
	stream byte
	data   []byte
	at     time.Time
}

var (
	errMissing        = errors.New("not found")
	errConflict       = errors.New("conflict")
	errInvalid        = errors.New("invalid request")
	errAlreadyRunning = errors.New("container is already running")
)

// VMOptions describes host services attached to a VM-backed engine.
type VMOptions struct {
	Rosetta       bool
	EngineSocket  string
	WindowsMounts bool
	Shares        ShareRoots
	Disk          bool
	HostCgroups   bool
	Relay         guestwire.RelayDial
	Archive       guestwire.RelayDial
	CPUs          uint
	MemoryMiB     uint64
	Events        *EventBus
	Version       string
	Builder       string
	BuildMirror   *BuildMirror
	Proxy         *Proxy
	Domains       *Domains
}

func New(ctx context.Context, conn net.Conn, images map[string]Image, state *State, vm VMOptions) (*Engine, error) {
	containers, err := state.restore()
	if err != nil {
		return nil, err
	}
	networks, err := state.restoreNetworks()
	if err != nil {
		return nil, err
	}
	for _, c := range containers {
		for _, ep := range c.endpoints() {
			if !guestwire.CustomBridge(ep.Network.Bridge) {
				continue
			}
			n, ok := networks[ep.Name]
			if !ok || n.ID != ep.ID || n.Bridge != ep.Network.Bridge ||
				!n.matchesEndpoint(ep.Network) {
				return nil, fmt.Errorf("container %s references an invalid network", c.id)
			}
		}
	}
	index, err := state.restoreImages()
	if err != nil {
		return nil, err
	}
	catalog := make(map[string]Image, len(images))
	for ref, image := range images {
		catalog[ref] = identifyImage(image)
	}
	e := &Engine{ctx: ctx, sharedEvents: vm.Events, images: index.references(catalog), catalog: catalog, imageIndex: index, containers: containers, networks: networks, state: state, settled: make(chan struct{}), remoteGuest: runtime.GOOS == "darwin", hostCgroups: vm.HostCgroups, guestShares: vm.Shares, guestDisk: vm.Disk, guestRelay: vm.Relay, domains: vm.Domains, guestArchive: vm.Archive, release: vm.Version, builderName: vm.Builder, vmCPUs: vm.CPUs, vmMemory: vm.MemoryMiB}
	e.proxy = vm.Proxy
	e.buildMirror = vm.BuildMirror
	e.rosetta = vm.Rosetta
	e.engineSocket, e.windowsMounts = vm.EngineSocket, vm.WindowsMounts
	if err := e.restoreBridgeIPv6(); err != nil {
		return nil, err
	}
	if conn == nil {
		e.closed = true
		close(e.settled)
		return e, nil
	}
	e.session = guestwire.NewSession(conn, e.onEvent)
	e.mu.Lock()
	for _, c := range e.containers {
		if c.canRestart(true) {
			e.queueRestart(c, true, 100*time.Millisecond)
		}
	}
	e.mu.Unlock()
	go func() {
		defer close(e.settled)
		<-e.session.Done()
		e.mu.Lock()
		defer e.mu.Unlock()
		e.closed = true
		for _, c := range e.containers {
			c.cancelRestart()
		}
		for _, x := range e.execs {
			if x.running {
				e.finishExec(x, 255)
			}
		}
		for _, c := range e.containers {
			if c.state == "starting" || c.state == "running" {
				e.finish(c, 255, e.session.Err())
			} else {
				c.closeOutputListeners()
			}
		}
	}()
	if e.remoteGuest {
		go e.syncGuestClock()
	}
	return e, nil
}

// The VM wall clock can fall behind while its Mac sleeps.
func (e *Engine) syncGuestClock() {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-e.ctx.Done():
			return
		case <-e.session.Done():
			return
		case <-ticker.C:
			if err := e.session.SetTime(e.ctx, time.Now()); err != nil {
				return
			}
		}
	}
}

// Done closes when the guest connection is gone and active runs are finalized.
func (e *Engine) Done() <-chan struct{} { return e.settled }

func (e *Engine) GuestError() error { return e.session.Err() }

// PrepareIdleShutdown reserves an idle engine for shutdown. Holding e.mu
// through the state transition excludes a pending container restart.
func (e *Engine) PrepareIdleShutdown() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed || e.builderUsers != 0 || e.volumeCleanups != 0 {
		return false
	}
	for _, c := range e.containers {
		if c.state == "starting" || c.restartTimer != nil {
			return false
		}
		if c.state == "running" && !(c.paused && isBuildKit(c)) {
			return false
		}
	}
	for _, x := range e.execs {
		if x.running {
			return false
		}
	}
	e.closed = true
	return true
}

func (e *Engine) Close() {
	if e.session == nil {
		return
	}
	e.mu.Lock()
	e.closed = true
	for _, c := range e.containers {
		c.cancelRestart()
	}
	e.mu.Unlock()
	if e.remoteGuest {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		_ = e.session.Shutdown(ctx)
		cancel()
	}
	_ = e.session.Close()
	<-e.settled
}

func (e *Engine) save(c *container) error {
	if e.state == nil {
		return nil
	}
	if err := e.state.save(c); err != nil {
		return fmt.Errorf("save container %s: %w", c.id, err)
	}
	return nil
}

// finish is called under e.mu. Waiters see an error if the terminal
// state could not be recorded on disk.
func (e *Engine) finish(c *container, code int32, failure error) {
	attempt := c.attempt
	c.attempt = nil
	c.paused = false
	c.state, c.exit, c.failure = "exited", code, failure
	c.finishedAt, c.vmStopped = time.Now(), e.closed
	if c.health != nil {
		c.health.Status = healthUnhealthy
	}
	if !attempt.startedAt.IsZero() {
		if c.oomKilled {
			e.emitContainer(c, "oom", nil)
		}
		e.emitContainer(c, "die", map[string]string{"exitCode": strconv.FormatInt(int64(code), 10)})
	}
	var err error
	autoRemove := c.run.Flags&guestwire.RunAutoRemove != 0 && !c.removing
	if autoRemove {
		err = e.deleteRecord(c)
		if err == nil {
			e.finishAutoRemoval(c)
		}
	} else {
		err = e.save(c)
	}
	if err != nil {
		log.Print(err)
		c.failure = errors.Join(c.failure, err)
		if autoRemove && c.removed != nil {
			close(c.removed)
			c.removed = nil
		}
	}
	attempt.code, attempt.failure = c.exit, c.failure
	attempt.close()
	c.closeOutputListeners()
	if err == nil && failure == nil && !e.closed && !attempt.startedAt.IsZero() && c.canRestart(false) {
		if c.restartDelay == 0 || time.Since(attempt.startedAt) >= 10*time.Second {
			c.restartDelay = 100 * time.Millisecond
		}
		e.queueRestart(c, false, c.restartDelay)
		c.restartDelay = min(c.restartDelay*2, 30*time.Second)
	}
}

func (e *Engine) create(name string, cfg Config) (string, error) {
	target, join := strings.CutPrefix(cfg.network, containerNetworkPrefix)
	if join {
		switch {
		case target == "":
			return "", fmt.Errorf("%w: invalid network mode: expected container:<name|id>", errInvalid)
		case cfg.Hostname != "":
			return "", fmt.Errorf("%w: conflicting options: hostname and the network mode", errInvalid)
		case len(cfg.links) != 0:
			return "", fmt.Errorf("%w: conflicting options: container type network can't be used with links", errInvalid)
		case !cfg.dns.Empty():
			return "", fmt.Errorf("%w: conflicting options: dns and the network mode", errInvalid)
		case len(cfg.extraHosts) != 0:
			return "", fmt.Errorf("%w: conflicting options: custom host-to-IP mapping and the network mode", errInvalid)
		case len(cfg.portBindings) != 0 || cfg.publishAll:
			return "", fmt.Errorf("%w: conflicting options: port publishing and the container type network mode", errInvalid)
		case len(cfg.ExposedPorts) != 0:
			return "", fmt.Errorf("%w: conflicting options: port exposing and the container type network mode", errInvalid)
		}
	}
	if cfg.network == "host" && len(cfg.links) != 0 {
		return "", fmt.Errorf("%w: conflicting options: host type networking can't be used with links", errInvalid)
	}
	if err := guestwire.ValidateDomainname(cfg.Domainname); err != nil {
		return "", fmt.Errorf("%w: %v", errInvalid, err)
	}
	if _, err := guestwire.ParseSysctls(cfg.sysctls, !join && cfg.network != "host"); err != nil {
		return "", fmt.Errorf("%w: %v", errInvalid, err)
	}
	if cfg.privileged && !e.remoteGuest && !e.hostCgroups {
		return "", fmt.Errorf("%w: privileged containers require delegated cgroups", errInvalid)
	}
	if cfg.autoRemove && cfg.restart.Name != "" && cfg.restart.Name != "no" {
		return "", fmt.Errorf("%w: auto-remove conflicts with restart policy", errInvalid)
	}
	e.imageOps.Lock()
	defer e.imageOps.Unlock()
	e.mu.Lock()
	selected, err := e.findImage(cfg.Image)
	e.mu.Unlock()
	if err != nil {
		return "", err
	}
	image := selected.Image
	platform := image.platform()
	if cfg.platform != "" {
		wanted, err := parseImagePlatform(cfg.platform)
		if err != nil {
			return "", err
		}
		if !matchesImagePlatform(platform, wanted) {
			return "", fmt.Errorf("%w: no such image: %s for %s", errMissing, cfg.Image, wanted)
		}
	}
	if err := e.checkExecutionPlatform(platform); err != nil {
		return "", err
	}
	inherited, shell, err := image.healthMetadata()
	if err != nil {
		return "", err
	}
	check, err := mergeHealthConfig(cfg.Healthcheck, inherited)
	if err != nil {
		return "", err
	}
	if len(cfg.Shell) != 0 {
		shell = cfg.Shell
	}
	exposed, bindings := mergePorts(image.ExposedPorts, cfg.ExposedPorts, cfg.portBindings, cfg.publishAll)
	if len(bindings) != 0 && !cfg.bridge && cfg.network != "host" {
		return "", fmt.Errorf("%w: port publishing requires a bridge or host network", errInvalid)
	}
	if e.remoteGuest && e.guestRelay == nil && len(bindings) != 0 {
		return "", fmt.Errorf("%w: this VM does not yet support published ports", errInvalid)
	}
	cfg.Entrypoint, cfg.Cmd = mergeCommand(cfg.Entrypoint, cfg.Cmd, image.Entrypoint, image.Cmd)
	argv := append(append([]string(nil), cfg.Entrypoint...), cfg.Cmd...)
	if len(argv) == 0 || argv[0] == "" {
		return "", fmt.Errorf("%w: no command specified", errInvalid)
	}
	if cfg.WorkingDir == "" {
		cfg.WorkingDir = image.WorkingDir
	}
	if cfg.User == "" {
		cfg.User = image.User
	}
	if cfg.StopSignal == "" {
		cfg.StopSignal = image.StopSignal
	}
	signal := sigTerm
	if cfg.StopSignal != "" {
		var err error
		signal, err = parseSignal(cfg.StopSignal)
		if err != nil {
			return "", err
		}
	}
	timeout := int64(10)
	if cfg.StopTimeout != nil {
		timeout = *cfg.StopTimeout
		if !validTimeout(timeout) {
			return "", fmt.Errorf("%w: invalid stop timeout", errInvalid)
		}
	}
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	id := hex.EncodeToString(raw[:])
	if name == "" {
		name = id[:12]
	}
	if cfg.Hostname == "" {
		cfg.Hostname = id[:12]
	}
	caps, err := cfg.capabilities.mask()
	if err != nil {
		return "", fmt.Errorf("%w: %v", errInvalid, err)
	}
	security, err := securityFlags(cfg.securityOpt)
	if err != nil {
		return "", err
	}
	run := guestwire.RunSpec{
		Caps: caps,
		ID:   id, Argv: argv, Env: mergeEnv(image.Env, cfg.Env),
		Cwd: cfg.WorkingDir, User: cfg.User, Hostname: cfg.Hostname, Domainname: cfg.Domainname, Flags: guestwire.RunNetworkNone | security, Limits: cfg.limits,
		DNS: cfg.dns, Process: cfg.process, Sysctls: cfg.sysctls, Devices: cfg.devices,
	}
	if !join {
		run.Env, run.Options = e.proxy.containerEnv(e.remoteGuest, cfg.network == "host", run.Env)
	}
	if cfg.privileged {
		run.Flags |= guestwire.RunPrivileged
	}
	if cfg.readOnlyRoot {
		run.Flags |= guestwire.RunReadOnlyRoot
	}
	if cfg.Tty {
		run.Flags |= guestwire.RunTTY
		if !slices.ContainsFunc(run.Env, func(env string) bool { return strings.HasPrefix(env, "TERM=") }) {
			run.Env = append(run.Env, "TERM=xterm")
		}
	}
	if cfg.openStdin {
		run.Flags |= guestwire.RunStdin
	}
	if cfg.autoRemove {
		run.Flags |= guestwire.RunAutoRemove
	}
	// Catalog layers are OCI order (base first), while the guest stacks
	// its root filesystem from the top layer down to the base.
	for i := len(image.Layers) - 1; i >= 0; i-- {
		run.Layers = append(run.Layers, image.Layers[i].Digest)
	}
	e.networkOps.Lock()
	defer e.networkOps.Unlock()
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return "", errors.New("daemon is shutting down")
	}
	for _, other := range e.containers {
		if other.name == name {
			return "", fmt.Errorf("%w: container name %q already in use", errConflict, name)
		}
	}
	mode := cfg.network
	if join {
		owner, err := e.find(target)
		if errors.Is(err, errMissing) {
			return "", fmt.Errorf("%w: No such container: %s", errMissing, target)
		}
		if err != nil {
			return "", err
		}
		mode = containerNetworkPrefix + owner.id
	}
	if cfg.pidMode != "" {
		owner, err := e.find(cfg.pidMode)
		if errors.Is(err, errMissing) {
			return "", fmt.Errorf("%w: No such container: %s", errMissing, cfg.pidMode)
		}
		if err != nil {
			return "", err
		}
		run.Options = append(slices.Clone(run.Options), guestwire.PIDJoinPrefix+owner.id)
	}
	if mode == "" {
		mode = "none"
		if cfg.bridge {
			mode = "bridge"
		}
	}
	links, err := e.resolveLinks(cfg.links, "")
	if err != nil {
		return "", err
	}
	endpoints, err := e.initialEndpoints(mode, cfg.networkEndpoints, cfg.networkMAC, name, id)
	if err != nil {
		return "", err
	}
	for _, key := range slices.Sorted(maps.Keys(cfg.networkEndpoints)) {
		values := cfg.networkEndpoints[key].links
		if len(values) == 0 {
			continue
		}
		network, err := e.network(key)
		if err != nil {
			return "", err
		}
		scoped, err := e.resolveLinks(values, network.ID)
		if err != nil {
			return "", err
		}
		links = append(links, scoped...)
	}
	for _, bind := range cfg.mounts {
		source := bind.spec.Source
		if !e.isEngineSocket(source) {
			switch {
			case e.remoteGuest:
				if _, err := e.guestShares.source(source, bind.create); err != nil {
					return "", err
				}
			default:
				_, err := os.Stat(source)
				if errors.Is(err, os.ErrNotExist) && bind.create {
					err = os.MkdirAll(source, 0755)
				}
				if err != nil {
					return "", bindSourceError(source, err)
				}
			}
		}
		mount, err := bind.spec.Encode()
		if err != nil {
			return "", fmt.Errorf("%w: %v", errInvalid, err)
		}
		run.Mounts = append(run.Mounts, mount)
	}
	for _, mount := range cfg.tmpfs {
		value, err := mount.Encode()
		if err != nil {
			return "", fmt.Errorf("%w: %v", errInvalid, err)
		}
		run.Mounts = append(run.Mounts, value)
	}
	volumeSpecs, err := declaredVolumes(cfg, image)
	if err != nil {
		return "", err
	}
	volumeMounts, volumeNames, anonymousVolumes, err := e.mountVolumes(volumeSpecs)
	if err != nil {
		return "", err
	}
	run.Mounts = append(run.Mounts, volumeMounts...)
	c := &container{
		id: id, name: name, image: cfg.Image, imageID: image.ID, created: time.Now().Unix(), run: run, blobs: image.Layers, state: "created", stopSignal: signal, stopTimeout: timeout, volumeNames: volumeNames,
		anonymousVolumes: anonymousVolumes,
		exposedPorts:     exposed, portBindings: bindings, consoleSize: cfg.consoleSize, logConfig: cfg.logConfig, labels: cfg.Labels, restartPolicy: cfg.restart,
		init: cfg.init, capabilities: cfg.capabilities, securityOpt: cfg.securityOpt,
	}
	c.entrypoint = append([]string{}, cfg.Entrypoint...)
	c.healthcheck, c.shell = check, shell
	c.networkName = mode
	c.links = links
	c.setEndpoints(endpoints)
	c.run.Network.ExtraHosts = cfg.extraHosts
	if err := e.save(c); err != nil {
		return "", err
	}
	e.containers[id] = c
	e.emitContainer(c, "create", nil)
	return id, nil
}

func mergeEnv(base, overrides []string) []string {
	env := append([]string(nil), base...)
	for _, value := range overrides {
		key, _, _ := strings.Cut(value, "=")
		for i := range env {
			name, _, _ := strings.Cut(env[i], "=")
			if name == key {
				env = append(env[:i], env[i+1:]...)
				break
			}
		}
		env = append(env, value)
	}
	return env
}

// find is called with e.mu held.
func (e *Engine) find(key string) (*container, error) {
	if c := e.containers[key]; c != nil {
		return c, nil
	}
	for _, c := range e.containers {
		if c.name == key {
			return c, nil
		}
	}
	var match *container
	for id, c := range e.containers {
		if strings.HasPrefix(id, key) {
			if match != nil {
				return nil, fmt.Errorf("%w: ambiguous container ID %q", errConflict, key)
			}
			match = c
		}
	}
	if match == nil {
		return nil, fmt.Errorf("%w: container %q", errMissing, key)
	}
	return match, nil
}

// lockOperation serializes lifecycle changes for one container without
// holding the engine lock while waiting for guest replies or events.
func (e *Engine) lockOperation(key string) (*container, error) {
	for {
		e.mu.Lock()
		if e.closed {
			e.mu.Unlock()
			return nil, errors.New("daemon is shutting down")
		}
		c, err := e.find(key)
		if err != nil {
			e.mu.Unlock()
			return nil, err
		}
		if c.operation.TryLock() {
			e.mu.Unlock()
			return c, nil
		}
		probe := c.probeStarting
		e.mu.Unlock()
		if probe == nil {
			return nil, fmt.Errorf("%w: container operation already in progress", errConflict)
		}
		// Wait for an internal probe's startup to complete. A background
		// check must not make lifecycle requests fail with a conflict.
		select {
		case <-probe:
		case <-e.ctx.Done():
			return nil, e.ctx.Err()
		}
	}
}

func (e *Engine) start(key string) error {
	c, err := e.lockOperation(key)
	if err != nil {
		return err
	}
	defer c.operation.Unlock()
	e.mu.Lock()
	running := c.state == "running" && !c.removing
	e.mu.Unlock()
	if running {
		return errAlreadyRunning
	}
	return e.startContainer(c, false)
}

func (e *Engine) startContainer(c *container, automatic bool) error {
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return errors.New("daemon is shutting down")
	}
	if (c.state != "created" && c.state != "exited") || c.removing {
		e.mu.Unlock()
		return fmt.Errorf("%w: container cannot start in its current state", errConflict)
	}
	if c.imageID != "" {
		image, err := e.findImage(c.image)
		if err != nil || image.ID != c.imageID {
			image, err = e.findImage(c.imageID)
		}
		if err == nil {
			if err := e.checkExecutionPlatform(image.Image.platform()); err != nil {
				e.mu.Unlock()
				return err
			}
		}
	}
	target := c.networkTarget()
	var joinErr error
	var ownerRun *runAttempt
	proxyNetwork := containerProxyNetwork{host: c.networkMode() == "host", relayOptions: c.run.Options}
	if target != "" {
		owner := e.containers[target]
		if owner == nil {
			joinErr = fmt.Errorf("%w: No such container: %s", errMissing, target)
		} else if owner.state != "running" || owner.attempt == nil || owner.removing {
			joinErr = fmt.Errorf("%w: cannot join network of a non running container: %s", errConflict, target)
		} else {
			proxyNetwork = owner.attempt.proxyNetwork
			ownerRun = owner.attempt
		}
	}
	if pid := c.pidTarget(); pid != "" && joinErr == nil {
		owner := e.containers[pid]
		if owner == nil {
			joinErr = fmt.Errorf("%w: No such container: %s", errMissing, pid)
		} else if owner.state != "running" || owner.attempt == nil || owner.removing {
			joinErr = fmt.Errorf("%w: cannot join PID namespace of a non running container: %s", errConflict, pid)
		}
	}
	previous, code, failure, oom := c.state, c.exit, c.failure, c.oomKilled
	startedAt, finishedAt, vmStopped := c.startedAt, c.finishedAt, c.vmStopped
	count, stopped, delay := c.restartCount, c.manualStopped, c.restartDelay
	c.manualStopped = false
	if automatic {
		c.restartCount++
	} else {
		c.restartCount, c.restartDelay = 0, 0
	}
	c.state, c.exit, c.failure, c.oomKilled = "starting", 0, nil, false
	c.startedAt, c.finishedAt, c.vmStopped = time.Time{}, time.Time{}, false
	endpoints := c.endpoints()
	for i := range endpoints {
		if endpoints[i].Network.Bridge != "" {
			endpoints[i].Interface = fmt.Sprintf("eth%d", i)
		}
	}
	c.setEndpoints(endpoints)
	// The record says "starting", with the cleared stop intent, before the
	// guest is asked: a daemon crash before Started then restores it as
	// exited and its restart policy still applies. A --rm container has no
	// restart policy and its record goes when it exits, so it skips the
	// write, as with Kiri.
	if c.run.Flags&guestwire.RunAutoRemove == 0 {
		if err := e.save(c); err != nil {
			c.state, c.exit, c.failure, c.oomKilled = previous, code, failure, oom
			c.startedAt, c.finishedAt, c.vmStopped = startedAt, finishedAt, vmStopped
			c.restartCount, c.manualStopped, c.restartDelay = count, stopped, delay
			e.mu.Unlock()
			return err
		}
	}
	attempt := c.nextAttempt()
	attempt.settled = make(chan struct{})
	c.cancelRestart()
	if joinErr != nil {
		e.finish(c, 255, joinErr)
		e.mu.Unlock()
		return joinErr
	}
	attempt.proxyNetwork = proxyNetwork
	run, size := c.run, c.consoleSize
	if len(c.links) != 0 {
		hosts, err := e.linkHosts(c)
		if err != nil {
			e.finish(c, 255, err)
			e.mu.Unlock()
			return err
		}
		run.Network.ExtraHosts = append(slices.Clone(run.Network.ExtraHosts), hosts...)
	}
	mapped, err := e.runtimeSocketMounts(run.Mounts)
	if err != nil {
		e.finish(c, 255, err)
		e.mu.Unlock()
		return err
	}
	run.Mounts = mapped
	if c.init != nil && *c.init {
		run.Flags |= guestwire.RunInit
	}
	if target != "" {
		run.Network.Options = []string{guestwire.NetworkJoinPrefix + target}
	}
	run.Network = e.liveNetwork(run.Network)
	for _, ep := range c.extraNetworks {
		run.AdditionalNetworks = append(run.AdditionalNetworks, e.liveNetwork(ep.Network))
	}
	if e.remoteGuest {
		if e.guestRelay == nil && len(c.portBindings) != 0 {
			err := fmt.Errorf("%w: this VM does not yet support published ports", errInvalid)
			e.finish(c, 255, err)
			e.mu.Unlock()
			return err
		}
		if len(c.volumeNames) != 0 && !e.guestDisk {
			err := fmt.Errorf("%w: VM named volumes require a data disk", errInvalid)
			e.finish(c, 255, err)
			e.mu.Unlock()
			return err
		}
		mounts, err := mapGuestMounts(e.guestShares, run.Mounts)
		if err != nil {
			e.finish(c, 255, err)
			e.mu.Unlock()
			return err
		}
		run.Mounts = mounts
	}
	if len(c.portBindings) != 0 {
		ports, err := e.reservePorts(c.portBindings, c.networkAddress())
		if err != nil {
			e.finish(c, 255, err)
			e.mu.Unlock()
			return err
		}
		c.attempt.ports = ports
	}
	e.mu.Unlock()

	if target != "" {
		run.Env = e.proxy.joinedContainerEnv(e.remoteGuest, run.Env, proxyNetwork)
		e.mu.Lock()
		attempt.joinedEnv = run.Env
		e.mu.Unlock()
	}
	var pid uint32
	if e.managedBuilderName(c.name) != "" {
		err = e.writeBuilderConfig(c)
		run.Env = e.buildMirror.environment(run.Env)
	}
	if err == nil {
		err = resolveVolumeMounts(e.ctx, run.Mounts)
	}
	if err == nil && ownerRun != nil {
		// The guest takes the network from the owner's process, which
		// must have joined its own first.
		err = ownerRun.waitSettled(e.ctx)
	}
	if err == nil {
		pid, err = hostagent.StartImage(e.ctx, e.session, run, c.blobs)
	}
	if rows, cols := size[0], size[1]; err == nil && run.Flags&guestwire.RunTTY != 0 && rows != 0 && cols != 0 {
		err = e.session.Resize(e.ctx, c.id, rows, cols)
	}
	e.mu.Lock()
	if c.attempt == attempt && c.state == "starting" {
		if err == nil {
			e.markStarted(c, pid)
		} else {
			e.finish(c, 255, err)
		}
	}
	e.mu.Unlock()
	return err
}

func (e *Engine) onEvent(event guestwire.RunEvent) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if x := e.execs[event.ID]; x != nil {
		e.onExecEvent(x, event)
		return
	}
	c := e.containers[event.ID]
	if c == nil {
		return
	}
	switch event.Kind {
	case guestwire.StartedMessage:
		if c.state == "starting" {
			e.markStarted(c, event.PID)
		}
	case guestwire.SettledMessage:
		if c.attempt != nil {
			c.attempt.markSettled()
		}
	case guestwire.OutputMessage:
		if len(event.Data) == 0 {
			return
		}
		entry := record{stream: event.Stream, data: slices.Clone(event.Data), at: time.Now().UTC()}
		c.appendLog(entry)
		for listener := range c.listeners {
			if err := listener.enqueue(entry); err != nil {
				delete(c.listeners, listener)
			}
		}
	case guestwire.ExitedMessage:
		if c.state == "starting" || c.state == "running" {
			c.oomKilled = event.Exit.OOMKilled
			e.finish(c, event.Exit.Code, nil)
		}
	}
}

// prepareWait registers a wait before the API sends its response headers.
func (e *Engine) prepareWait(key, condition string) (func(context.Context) (int32, error), error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	c, err := e.find(key)
	if err != nil {
		return nil, err
	}
	if condition == "removed" {
		if c.state == "exited" && !c.removing && c.run.Flags&guestwire.RunAutoRemove != 0 && c.failure != nil {
			failure := c.failure
			return func(context.Context) (int32, error) { return 255, failure }, nil
		}
		if c.removed == nil {
			c.removed = make(chan struct{})
		}
		removed := c.removed
		return func(ctx context.Context) (int32, error) {
			select {
			case <-removed:
			case <-e.settled:
			case <-ctx.Done():
				return 0, ctx.Err()
			}
			e.mu.Lock()
			defer e.mu.Unlock()
			if c.state != "removed" {
				return 255, errors.Join(c.failure, errors.New("container was not removed"))
			}
			return c.exit, c.failure
		}, nil
	}
	if c.state == "created" && condition != "next-exit" {
		return func(context.Context) (int32, error) { return 0, nil }, nil
	}
	if c.state == "exited" && condition != "next-exit" {
		code, failure := c.exit, c.failure
		return func(context.Context) (int32, error) { return code, failure }, nil
	}
	attempt := c.nextAttempt()
	return func(ctx context.Context) (int32, error) {
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-attempt.done:
		case <-e.settled:
			select {
			case <-attempt.done:
			default:
				return 255, errors.New("daemon stopped before the container exited")
			}
		}
		return attempt.code, attempt.failure
	}, nil
}
