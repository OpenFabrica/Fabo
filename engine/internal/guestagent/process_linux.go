package guestagent

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"

	"github.com/OpenFabrica/Fabo/engine/internal/guestwire"
)

// Runtime owns the agent's live child processes and writable layers.
type Runtime struct {
	dir          string
	cgroupDir    string
	proxySocket  string
	layers       *LayerStore
	mu           sync.Mutex
	children     map[string]*childProcess
	wait         sync.WaitGroup
	bridges      map[string]*bridgeNetwork
	forwarding   forwardingRefs
	vm           bool
	volumeRoot   string
	driverMounts map[string]string
}

func OpenRuntime(dir, cgroupDir string, layers *LayerStore, vm bool, volumeRoot, proxySocket string) (*Runtime, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	if volumeRoot != "" {
		if err := os.MkdirAll(volumeRoot, 0700); err != nil {
			return nil, err
		}
	}
	return &Runtime{dir: dir, cgroupDir: cgroupDir, proxySocket: proxySocket, layers: layers, children: make(map[string]*childProcess), bridges: make(map[string]*bridgeNetwork), forwarding: make(forwardingRefs), vm: vm, volumeRoot: volumeRoot, driverMounts: make(map[string]string)}, nil
}

type childProcess struct {
	*exec.Cmd
	input        *childInput
	terminal     *os.File
	ready        chan struct{}
	cgroup       string
	limits       guestwire.Limits
	network      *containerNetwork
	hostNetwork  bool
	hostname     string
	domainname   string
	networkFiles *networkFiles
	isExec       bool
	privileged   bool
	caps         uint64
	security     uint32
	execOutput   *execOutput
	// signalGroup sends signals to the child's whole process group
	// instead of the child alone; see processLaunch.group.
	signalGroup bool
	// setup is the child's bootstrap status pipe while a container that was
	// reported started early is still setting up; see processLaunch.early.
	setup *os.File
	// setupDone closes once such a container has entered its own root and
	// namespaces, or failed to; setupFailed is set before. Nil when the
	// child was reported started only after its exec. See lockSettled.
	setupDone   chan struct{}
	setupFailed bool
}

type processConfig struct {
	Spec              guestwire.RunSpec
	Root, Upper, Work string
	Lower             []string
	Cgroup            bool
	VM                bool
	JoinNetwork       bool
	DeviceNodes       []deviceNode
	HostDevices       []deviceNode
	// Early asks the child to report the end of its setup on the status pipe
	// with setupReady, for a container reported started before it.
	Early bool
}

func validContainerID(id string) bool {
	_, err := hex.DecodeString(id)
	return len(id) >= 12 && len(id) <= 64 && err == nil
}

func (r *Runtime) Run(spec guestwire.RunSpec, emit func(guestwire.RunEvent) error, ack func(string, uint32) error) error {
	start := time.Now()
	if !validContainerID(spec.ID) || len(spec.Argv) == 0 {
		return fmt.Errorf("invalid container ID or command")
	}
	if spec.Flags & ^(guestwire.RunNetworkHost|guestwire.RunNetworkNone|guestwire.RunAutoRemove|guestwire.RunReadOnlyRoot|guestwire.RunTTY|guestwire.RunStdin|guestwire.RunPrivileged|guestwire.RunInit|guestwire.RunSecurityFlags) != 0 {
		return fmt.Errorf("unsupported Go guest run options")
	}
	if err := guestwire.ValidateCapabilities(spec.Caps); err != nil {
		return err
	}
	pidTarget, runOptions := guestwire.SplitPIDJoin(spec.Options)
	if pidTarget != "" && (!validContainerID(pidTarget) || pidTarget == spec.ID) {
		return fmt.Errorf("invalid shared container PID namespace")
	}
	proxyPorts, err := windowsProxyPorts(runOptions, r.proxySocket)
	if err != nil {
		return err
	}
	if spec.Flags&guestwire.RunPrivileged != 0 && r.cgroupDir == "" {
		return fmt.Errorf("privileged containers require delegated cgroups")
	}
	hostNetwork := spec.Flags&guestwire.RunNetworkHost != 0
	target, err := joinNetworkTarget(spec)
	if err != nil {
		return err
	}
	if hostNetwork && (spec.Flags&guestwire.RunNetworkNone != 0 || len(proxyPorts) != 0) {
		return fmt.Errorf("host networking conflicts with isolated network options")
	}
	var networks []guestwire.Network
	if target != "" {
		if hostNetwork || spec.Flags&guestwire.RunNetworkNone != 0 || len(proxyPorts) != 0 || !spec.DNS.Empty() {
			return fmt.Errorf("shared container network conflicts with other network modes")
		}
	} else if hostNetwork || spec.Flags&guestwire.RunNetworkNone != 0 {
		if spec.Network.Bridge != "" || spec.Network.IP != [4]byte{} || spec.Network.Gateway != [4]byte{} || spec.Network.Prefix != 0 ||
			len(spec.Network.Options)+len(spec.Network.DNSNames)+len(spec.AdditionalNetworks) != 0 {
			return fmt.Errorf("host and none network modes cannot have bridge endpoints")
		}
	} else {
		primary := spec.Network
		primary.ExtraHosts = nil
		networks = append([]guestwire.Network{primary}, spec.AdditionalNetworks...)
		for _, n := range networks {
			if err := validateNetwork(n); err != nil {
				return err
			}
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.children[spec.ID]; exists {
		return fmt.Errorf("container is already running")
	}
	if err := r.ensureVolumeMounts(spec.Mounts); err != nil {
		return err
	}
	nodes, deviceRules, err := resolveDevices(spec.Devices)
	if err != nil {
		return err
	}
	cfg, err := r.prepareRoot(spec)
	if err != nil {
		return err
	}
	cfg.DeviceNodes = nodes
	if spec.Flags&guestwire.RunPrivileged != 0 {
		cfg.HostDevices, err = privilegedDeviceNodes()
		if err != nil {
			return fmt.Errorf("enumerate privileged container devices: %w", err)
		}
	}
	dir := filepath.Join(r.dir, spec.ID)
	var cgroup *os.File
	if cgroupLimits(spec.Limits) || r.cgroupDir != "" {
		var err error
		cgroup, err = createCgroup(r.cgroupDir, spec.ID, spec.Limits)
		if err != nil {
			return err
		}
		defer cgroup.Close()
		cfg.Cgroup = true
	}
	launched := false
	defer func() {
		if cgroup != nil && !launched {
			_ = os.Remove(cgroup.Name())
		}
	}()
	if spec.Flags&guestwire.RunPrivileged == 0 {
		if err := restrictDevices(cgroup, deviceRules); err != nil {
			if !spec.Devices.Empty() {
				return fmt.Errorf("apply custom device policy: %w", err)
			}
			fmt.Fprintf(os.Stderr, "fabo: device filter unavailable, dropping CAP_MKNOD: %v\n", err)
			cfg.Spec.Caps = guestwire.EncodeCapabilities(guestwire.EffectiveCapabilities(spec.Caps) &^ (1 << unix.CAP_MKNOD))
		}
	}
	var files []*os.File
	var etc *networkFiles
	if target == "" {
		etc, err = prepareNetworkFiles(cfg.Spec, cfg.VM, r.dir)
		if err != nil {
			return fmt.Errorf("prepare container network files: %w", err)
		}
		defer func() {
			if !launched {
				etc.release()
			}
		}()
	} else {
		owner := r.children[target]
		if owner == nil || owner.isExec {
			return fmt.Errorf("network container %s is not running", target)
		}
		// The network is taken from the target's process: it must have
		// joined its own first.
		if err := owner.requireSettled(target); err != nil {
			return err
		}
		ns, err := openSharedNetwork(owner)
		if err != nil {
			return err
		}
		defer ns.Close()
		files = []*os.File{ns}
		cfg.Spec.Hostname, cfg.JoinNetwork = owner.hostname, true
		cfg.Spec.Domainname = owner.domainname
		hostNetwork = owner.hostNetwork
		etc = owner.networkFiles
	}
	if err := etc.mount(&cfg.Spec); err != nil {
		return err
	}
	var network *containerNetwork
	attributes := &syscall.SysProcAttr{
		Cloneflags: unix.CLONE_NEWNS | unix.CLONE_NEWPID | unix.CLONE_NEWUTS | unix.CLONE_NEWIPC | unix.CLONE_NEWCGROUP,
	}
	// A container sharing another's PID namespace starts inside it instead
	// of a new one; the /proc it mounts then lists that container's
	// processes, and /proc/1/root is that container's root.
	var startIn func(*exec.Cmd) error
	if pidTarget != "" {
		owner := r.children[pidTarget]
		if owner == nil || owner.isExec {
			return fmt.Errorf("PID namespace container %s is not running", pidTarget)
		}
		pidNS, err := os.Open(fmt.Sprintf("/proc/%d/ns/pid", owner.Process.Pid))
		if err != nil {
			return fmt.Errorf("open target PID namespace: %w", err)
		}
		defer pidNS.Close()
		if err := owner.Process.Signal(syscall.Signal(0)); err != nil {
			return fmt.Errorf("check PID namespace container process: %w", err)
		}
		attributes.Cloneflags &^= unix.CLONE_NEWPID
		startIn = func(cmd *exec.Cmd) error { return startInPIDNamespace(cmd, pidNS) }
	}
	if !hostNetwork && target == "" {
		attributes.Cloneflags |= unix.CLONE_NEWNET
	}
	cfg.Early = spec.Flags&guestwire.RunTTY == 0
	launch := processLaunch{mode: "__fabo-container", id: spec.ID, flags: spec.Flags, config: cfg, attributes: attributes, cgroup: cgroup, files: files, start: startIn, early: cfg.Early}
	launch.prepare = func(pid int) error {
		if hostNetwork || target != "" {
			return nil
		}
		var err error
		network, err = openContainerNetwork(pid, r.vm, spec)
		if err != nil {
			return fmt.Errorf("prepare container network: %w", err)
		}
		for i, spec := range networks {
			if err := r.attachNetwork(network, cfg.Spec.ID, fmt.Sprintf("eth%d", i), spec); err != nil {
				return err
			}
		}
		return network.startWindowsProxy(r.proxySocket, proxyPorts)
	}
	child, err := launchProcess(launch, emit, ack)
	if err != nil {
		if network != nil {
			network.close()
		}
		return err
	}
	child.network = network
	child.hostNetwork = hostNetwork
	child.hostname = cfg.Spec.Hostname
	child.domainname = cfg.Spec.Domainname
	child.networkFiles = etc
	if target != "" {
		etc.refs++
	}
	child.limits = spec.Limits
	child.privileged = spec.Flags&guestwire.RunPrivileged != 0
	child.caps = cfg.Spec.Caps
	child.security = cfg.Spec.Flags & guestwire.RunSecurityFlags
	launched = true
	if cgroup != nil {
		child.cgroup = cgroup.Name()
	}
	return r.trackProcess(spec.ID, child, start, emit, func() bool {
		if network != nil {
			network.close()
		}
		if child.cgroup == "" {
			return false
		}
		oom := field(readText(filepath.Join(child.cgroup, "memory.events")), "oom_kill") != 0
		remove := os.Remove
		if child.privileged {
			remove = removeNestedCgroups
		}
		if err := remove(child.cgroup); err != nil {
			fmt.Fprintln(os.Stderr, "remove container cgroup:", err)
		}
		return oom
	}, func() {
		etc.release()
		if spec.Flags&guestwire.RunAutoRemove != 0 {
			_ = os.RemoveAll(dir)
		}
	})
}

// prepareRoot locates layers and creates the container's writable directories.
// The caller serializes filesystem operations for the container.
func (r *Runtime) prepareRoot(spec guestwire.RunSpec) (processConfig, error) {
	dir := filepath.Join(r.dir, spec.ID)
	cfg := processConfig{Spec: spec, VM: r.vm, Root: filepath.Join(dir, "root"), Upper: filepath.Join(dir, "upper"), Work: filepath.Join(dir, "work")}
	seen := make(map[string]bool, len(spec.Layers))
	for _, id := range spec.Layers {
		digest, err := layerDigest(id)
		if err != nil || !r.layers.has(digest) {
			return processConfig{}, fmt.Errorf("layer %q is missing", id)
		}
		// Like the Rust agent, use the topmost occurrence of a repeated layer.
		if seen[digest] {
			continue
		}
		seen[digest] = true
		cfg.Lower = append(cfg.Lower, filepath.Join(r.layers.dir, digest))
	}
	if len(cfg.Lower) == 0 {
		// OverlayFS still needs a lower directory for a scratch image.
		empty := filepath.Join(r.dir, ".empty-layer")
		if err := os.MkdirAll(empty, 0755); err != nil {
			return processConfig{}, err
		}
		cfg.Lower = []string{empty}
	}
	for _, p := range []string{cfg.Root, cfg.Work} {
		if err := os.MkdirAll(p, 0700); err != nil {
			return processConfig{}, err
		}
	}
	// Overlay uses upper's permissions for its root. Set the initial mode
	// explicitly because the daemon can start with a restrictive umask.
	if err := os.Mkdir(cfg.Upper, 0755); err == nil {
		if err := os.Chmod(cfg.Upper, 0755); err != nil {
			return processConfig{}, err
		}
	} else if !os.IsExist(err) {
		return processConfig{}, err
	}
	return cfg, nil
}

type processLaunch struct {
	mode, id   string
	flags      uint32
	config     any
	attributes *syscall.SysProcAttr
	cgroup     *os.File
	files      []*os.File
	start      func(*exec.Cmd) error
	prepare    func(int) error
	// group makes signals reach the child's whole process group. Every
	// child already leads its own session (and so its own group); this
	// only chooses whether a signal goes to that group or to the child.
	group bool
	// early reports the process started once its network is in place,
	// before the child has mounted its root and exec'd the command, as Kiri
	// does. A setup failure then ends the container with exit code 127 (126
	// for a command that cannot run) and the reason on its stderr, instead
	// of failing the start. A terminal is only received at the end of
	// setup, so TTY runs keep waiting for it.
	early bool
}

// launchProcess starts the agent's namespace child for a container or an
// exec, hands it its config and waits for its setup as p describes.
func launchProcess(p processLaunch, emit func(guestwire.RunEvent) error, ack func(string, uint32) error) (*childProcess, error) {
	data, err := json.Marshal(p.config)
	if err != nil {
		return nil, err
	}
	self, err := os.Executable()
	if err != nil {
		return nil, err
	}
	status, childStatus, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	keepStatus := false
	defer func() {
		if !keepStatus {
			_ = status.Close()
		}
	}()
	defer childStatus.Close()
	ready := make(chan struct{})
	cmd := exec.Command(self, p.mode)
	cmd.Env = []string{}
	if p.flags&guestwire.RunInit != 0 {
		// Leave SIGURG available for the container API instead of using it
		// for the Go runtime's own asynchronous preemption.
		cmd.Env = append(cmd.Env, "GODEBUG=asyncpreemptoff=1")
	}
	// The child waits for this config before entering its root. Configure
	// the veth first, while the child's network namespace is available.
	configReader, configWriter, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	defer configReader.Close()
	defer configWriter.Close()
	cmd.Stdin = configReader
	cmd.ExtraFiles = []*os.File{childStatus}
	tty := p.flags&guestwire.RunTTY != 0
	var terminalSocket int
	var childTerminal *os.File
	launched := false
	var terminal, inputReader, inputWriter *os.File
	if tty {
		pair, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_SEQPACKET|unix.SOCK_CLOEXEC, 0)
		if err != nil {
			return nil, err
		}
		terminalSocket = pair[0]
		defer unix.Close(terminalSocket)
		childTerminal = os.NewFile(uintptr(pair[1]), "terminal-bootstrap")
		defer childTerminal.Close()
		cmd.ExtraFiles = append(cmd.ExtraFiles, childTerminal)
	} else if p.flags&guestwire.RunStdin != 0 {
		inputReader, inputWriter, err = os.Pipe()
		if err != nil {
			return nil, err
		}
		defer inputReader.Close()
		defer func() {
			if !launched {
				_ = inputWriter.Close()
			}
		}()
		cmd.ExtraFiles = append(cmd.ExtraFiles, inputReader)
	}
	if !tty {
		cmd.Stdout = processOutput{ready, p.id, 1, emit}
		cmd.Stderr = processOutput{ready, p.id, 2, emit}
	}
	cmd.ExtraFiles = append(cmd.ExtraFiles, p.files...)
	// Every container and exec leads a session of its own, as runc gives
	// them. Otherwise it stays in the agent's process group, which on a
	// Linux host is the daemon's too: Apache prefork's stop
	// (kill(0, SIGTERM)) then reached the engine. A terminal also needs a
	// session leader to become its controlling terminal (TIOCSCTTY).
	// Setpgid must stay off: Go's child calls setsid before setpgid, and
	// a session leader cannot change its group (EPERM). setsid already
	// makes the child lead a group whose ID is its PID.
	p.attributes.Setsid = true
	p.attributes.Setpgid = false
	cmd.SysProcAttr = p.attributes
	if p.cgroup != nil {
		cmd.SysProcAttr.UseCgroupFD = true
		cmd.SysProcAttr.CgroupFD = int(p.cgroup.Fd())
	}
	start := p.start
	if start == nil {
		start = (*exec.Cmd).Start
	}
	if err := start(cmd); err != nil {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
			close(ready)
			_ = cmd.Wait()
		}
		return nil, err
	}
	_ = childStatus.Close()
	_ = configReader.Close()
	if childTerminal != nil {
		_ = childTerminal.Close()
	}
	if inputReader != nil {
		_ = inputReader.Close()
	}
	prepare := func() error {
		if p.prepare != nil {
			if err := p.prepare(cmd.Process.Pid); err != nil {
				return err
			}
		}
		if _, err := configWriter.Write(data); err != nil {
			return err
		}
		_ = configWriter.Close()
		if p.early {
			// trackProcess reads the outcome of the setup in the background.
			return nil
		}
		// The child keeps fd 3 until exec or reports its setup error.
		failure, err := io.ReadAll(io.LimitReader(status, 4096))
		if err != nil {
			return err
		}
		if len(failure) != 0 {
			return fmt.Errorf("container setup: %s", strings.TrimSpace(string(failure)))
		}
		if tty {
			terminal, err = receiveTerminal(terminalSocket)
			if err != nil {
				return fmt.Errorf("receive terminal: %w", err)
			}
		}
		return nil
	}
	if err := prepare(); err != nil {
		_ = cmd.Process.Kill()
		close(ready)
		_ = cmd.Wait()
		return nil, err
	}
	launched = true
	child := &childProcess{Cmd: cmd, terminal: terminal, ready: ready, signalGroup: p.group}
	if p.early {
		child.setup, keepStatus = status, true
		child.setupDone = make(chan struct{})
	}
	if terminal != nil && p.flags&guestwire.RunStdin != 0 {
		inputWriter = terminal
	}
	if inputWriter != nil {
		child.input = newChildInput(inputWriter, func(size uint32) error {
			return ack(p.id, size)
		})
	}
	return child, nil
}

// trackProcess publishes Started before output, and drains output before Exited.
// The caller holds the runtime mutex while registering the child.
func (r *Runtime) trackProcess(id string, child *childProcess, start time.Time, emit func(guestwire.RunEvent) error, cleanup func() bool, remove func()) error {
	cloned := time.Now()
	r.children[id] = child
	r.wait.Add(1)
	emitErr := emit(guestwire.RunEvent{Kind: guestwire.StartedMessage, ID: id, PID: uint32(child.Process.Pid)})
	if emitErr == nil && !child.isExec && child.setupDone == nil {
		// Started only after its exec: the container is already settled.
		emitErr = emit(guestwire.RunEvent{Kind: guestwire.SettledMessage, ID: id})
	}
	close(child.ready)
	var terminalDone chan struct{}
	if terminal := child.terminal; terminal != nil {
		terminalDone = make(chan struct{})
		go func() {
			defer close(terminalDone)
			out := processOutput{child.ready, id, 1, emit}
			if _, err := io.Copy(out, terminalOutput{terminal}); err != nil {
				// A failed event write must not strand the child in a
				// full terminal buffer while the session shuts down.
				_ = terminal.Close()
			}
		}()
	}
	go func() {
		defer r.wait.Done()
		var setupFailure string
		if child.setup != nil {
			// The child holds this close-on-exec pipe until it execs the
			// command, or writes why it could not and exits.
			setupFailure = child.readSetup(func() {
				// The host waits for this before exec or a network join.
				_ = emit(guestwire.RunEvent{Kind: guestwire.SettledMessage, ID: id})
			})
		}
		err := child.Wait()
		if terminalDone != nil {
			<-terminalDone
			_ = child.terminal.Close()
		}
		if child.input != nil {
			child.input.abort()
		}
		code := int32(0)
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			st := exit.Sys().(syscall.WaitStatus)
			if st.Signaled() {
				code = int32(128 + st.Signal())
			} else {
				code = int32(st.ExitStatus())
			}
		} else if err != nil {
			code = 255
		}
		if setupFailure != "" {
			// After the child's own output, which Wait has drained.
			_ = emit(guestwire.RunEvent{Kind: guestwire.OutputMessage, ID: id, Stream: 2, Data: []byte("fabo: cannot start container: " + setupFailure + "\n")})
		}
		var oom bool
		if cleanup != nil {
			oom = cleanup()
		}
		r.mu.Lock()
		delete(r.children, id)
		if remove != nil {
			remove()
		}
		_ = emit(guestwire.RunEvent{Kind: guestwire.ExitedMessage, ID: id, Exit: guestwire.Exit{
			Code: code, OOMKilled: oom, SetupNS: uint64(cloned.Sub(start)), RunNS: uint64(time.Since(cloned)),
		}})
		r.mu.Unlock()
	}()
	return emitErr
}

type processOutput struct {
	ready  <-chan struct{}
	id     string
	stream byte
	emit   func(guestwire.RunEvent) error
}

func (w processOutput) Write(data []byte) (int, error) {
	<-w.ready
	if err := w.emit(guestwire.RunEvent{Kind: guestwire.OutputMessage, ID: w.id, Stream: w.stream, Data: data}); err != nil {
		return 0, err
	}
	return len(data), nil
}

func (r *Runtime) Signal(id string, signal uint32) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if child := r.children[id]; child != nil {
		child.signal(syscall.Signal(signal))
	}
}

// signal delivers signal to the child, or to its whole process group when
// it was launched as a group (a private exec's descendants go with it).
func (child *childProcess) signal(signal syscall.Signal) {
	if child.signalGroup {
		_ = syscall.Kill(-child.Process.Pid, signal)
		return
	}
	_ = child.Process.Signal(signal)
}

func (r *Runtime) Remove(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !validContainerID(id) || r.children[id] != nil {
		return fmt.Errorf("cannot remove container")
	}
	return os.RemoveAll(filepath.Join(r.dir, id))
}

func (r *Runtime) Close() {
	r.mu.Lock()
	for _, child := range r.children {
		if child.execOutput != nil {
			child.execOutput.grant(0)
		}
		child.signal(syscall.SIGKILL)
	}
	r.mu.Unlock()
	r.wait.Wait()
	// The exited containers' veths are deleted in the background; finish
	// that before the bridges go and, on a Linux host, before the agent exits.
	retiredVeths.wait()
	for name := range r.driverMounts {
		if err := r.releaseDriverVolume(name); err != nil {
			fmt.Fprintln(os.Stderr, "unmount local volume", name+":", err)
		}
	}
	for name, bridge := range r.bridges {
		if err := bridge.close(); err != nil {
			fmt.Fprintln(os.Stderr, "remove bridge", name+":", err)
		}
	}
}

// deleteVeth deletes link now, treating an already removed link as success.
func deleteVeth(link netlink.Link) error {
	if link == nil {
		return nil
	}
	if err := netlink.LinkDel(link); err != nil && !vethGone(err) {
		return err
	}
	return nil
}

// retireVeth takes an exited container's veth off its bridge at once and
// deletes it in the background; see vethRetirer.
func retireVeth(link netlink.Link) {
	retiredVeths.retire(link)
}
