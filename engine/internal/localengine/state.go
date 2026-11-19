//go:build linux || darwin

package localengine

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/OpenFabrica/Fabo/engine/internal/guestwire"
	"github.com/OpenFabrica/Fabo/engine/internal/hostagent"
	"github.com/OpenFabrica/Fabo/engine/internal/imagescan"
)

// State owns the exclusive lock for a private daemon state directory.
type State struct {
	dir               string
	lock              *os.File
	scanner           *imagescan.Scanner
	builderSettingsMu sync.Mutex
}

type savedContainer struct {
	ImageID                                  string            `json:",omitempty"`
	Labels                                   map[string]string `json:",omitempty"`
	StopSignal                               uint32
	StopTimeout                              *int64
	ConsoleSize                              [2]uint16
	LogConfig                                *logConfig `json:",omitempty"`
	ID, Name, Image                          string
	NetworkName, NetworkID, NetworkInterface string
	ExtraNetworks                            []containerEndpoint `json:",omitempty"`
	Links                                    []containerLink     `json:",omitempty"`
	Created                                  int64
	StartedAt                                time.Time        `json:",omitzero"`
	FinishedAt                               time.Time        `json:",omitzero"`
	VMStopped                                bool             `json:",omitempty"`
	Capabilities                             capabilityConfig `json:",omitzero"`
	SecurityOpt                              []string
	Run                                      guestwire.RunSpec
	Init                                     *bool `json:",omitempty"`
	Entrypoint                               []string
	Blobs                                    []hostagent.Layer
	State                                    string
	Healthcheck                              *healthConfig    `json:",omitempty"`
	Shell                                    []string         `json:",omitempty"`
	Health                                   *containerHealth `json:",omitempty"`
	RestartPolicy                            restartPolicy    `json:",omitzero"`
	RestartCount                             int              `json:",omitempty"`
	ManualStopped                            bool             `json:",omitempty"`
	Exit                                     int32
	Failure                                  string
	Logs                                     []savedRecord
	OOMKilled                                bool
	VolumeNames                              []string
	AnonymousVolumes                         []string            `json:",omitempty"`
	ExposedPorts                             map[string]struct{} `json:",omitempty"`
	PortBindings                             []portBinding       `json:",omitempty"`
}

type savedRecord struct {
	Stream byte
	Data   []byte
	At     time.Time `json:",omitzero"`
}

func OpenState(dir string) (*State, error) {
	info, err := os.Stat(dir)
	if err != nil {
		return nil, err
	}
	owner, ok := info.Sys().(*syscall.Stat_t)
	if !info.IsDir() || info.Mode().Perm()&0077 != 0 || !ok || owner.Uid != uint32(os.Geteuid()) {
		return nil, fmt.Errorf("state directory must be private and owned by the daemon user")
	}
	lock, err := os.OpenFile(filepath.Join(dir, "daemon.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = lock.Close()
		return nil, fmt.Errorf("lock daemon state: %w", err)
	}
	return &State{dir: dir, lock: lock, scanner: imagescan.New(dir)}, nil
}

func (s *State) Close() error { return s.lock.Close() }

func (s *State) save(c *container) error {
	rec := savedContainer{
		StopSignal: c.stopSignal, StopTimeout: &c.stopTimeout, ID: c.id, Name: c.name, Image: c.image, ImageID: c.imageID, Created: c.created, Run: c.run, Blobs: c.blobs, State: c.state, Exit: c.exit, OOMKilled: c.oomKilled, VolumeNames: c.volumeNames,
		AnonymousVolumes: c.anonymousVolumes,
		ExposedPorts:     c.exposedPorts, PortBindings: c.portBindings, ConsoleSize: c.consoleSize, LogConfig: savedLogConfig(c.logConfig), NetworkName: c.networkName, NetworkID: c.networkID, NetworkInterface: c.networkInterface, ExtraNetworks: c.extraNetworks, Labels: c.labels,
		Healthcheck: c.healthcheck, Shell: c.shell, Health: c.health, Entrypoint: c.entrypoint, Init: c.init, Capabilities: c.capabilities, SecurityOpt: append([]string{}, c.securityOpt...),
		RestartPolicy: c.restartPolicy, RestartCount: c.restartCount, ManualStopped: c.manualStopped, Links: c.links,
		StartedAt: c.startedAt, FinishedAt: c.finishedAt, VMStopped: c.vmStopped,
	}
	if c.failure != nil {
		rec.Failure = c.failure.Error()
	}
	for _, entry := range c.logs {
		rec.Logs = append(rec.Logs, savedRecord{Stream: entry.stream, Data: entry.data, At: entry.at})
	}
	return saveJSON(s.dir, c.id+".json", rec)
}

func saveJSON(dir, name string, value any) error {
	tmp, err := os.CreateTemp(dir, ".record-")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	if err := json.NewEncoder(tmp).Encode(value); err != nil {
		return err
	}
	// Match the VM disk policy: fsync, not Darwin File.Sync's stronger
	// F_FULLFSYNC. Keep write/sync/close failures ahead of atomic replacement.
	if err := syscall.Fsync(int(tmp.Fd())); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), filepath.Join(dir, name))
}

// restoreImages reads the image index. An index that cannot be read or
// parsed is set aside (setAside) and replaced by an empty one, so the engine
// still starts; the copy keeps layer cleanup off (damagedFiles).
func (s *State) restoreImages() (imageIndex, error) {
	raw, err := os.ReadFile(filepath.Join(s.dir, "image-index.json"))
	if err == nil {
		var index imageIndex
		if err = json.Unmarshal(raw, &index); err == nil {
			return index, nil
		}
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		if err := s.setAsideIndex("image-index.json", err); err != nil {
			return imageIndex{}, err
		}
		// Write the empty index at once: with no index at all, the next
		// start would import the legacy images.json instead.
		if err := saveJSON(s.dir, "image-index.json", imageIndex{}); err != nil {
			log.Printf("save empty image index: %v", err)
		}
		return imageIndex{}, nil
	}
	// Import the previous registry-only file when the new index is absent.
	raw, err = os.ReadFile(filepath.Join(s.dir, "images.json"))
	if errors.Is(err, os.ErrNotExist) {
		return imageIndex{}, nil
	}
	var images map[string]Image
	if err == nil {
		err = json.Unmarshal(raw, &images)
	}
	if err != nil {
		return imageIndex{}, s.setAsideIndex("images.json", err)
	}
	index := imageIndex{Overrides: make(map[string]*Image, len(images))}
	for ref, image := range images {
		value := identifyImage(image)
		index.Overrides[ref] = &value
	}
	return index, nil
}

// setAsideIndex moves a damaged index file aside after cause made it
// unusable. If it cannot be moved the start fails as before: going on would
// let the next change overwrite the only copy with an empty index.
func (s *State) setAsideIndex(name string, cause error) error {
	copy, err := s.setAside(name)
	if err != nil {
		return fmt.Errorf("read %s: %w (and could not set it aside: %v)", name, cause, err)
	}
	log.Printf("%s is damaged (%v): kept as %s and started with an empty image list; unused image layers are kept until that copy is removed", name, cause, filepath.Join(s.dir, copy))
	return nil
}

// corruptMark separates a damaged state file's name from the time it was
// set aside: <name>.corrupt-<time>.
const corruptMark = ".corrupt-"

// setAside renames a damaged state file to <name>.corrupt-<time> in the same
// directory and returns the new name. It never deletes: the bytes stay for
// whoever repairs them, and their presence keeps destructive cleanup off.
func (s *State) setAside(name string) (string, error) {
	stamp := time.Now().UTC().Format("20060102T150405.000000000Z")
	for i := 0; ; i++ {
		target := name + corruptMark + stamp
		if i > 0 {
			target += "-" + strconv.Itoa(i)
		}
		// rename(2) replaces an existing file; never replace an earlier copy.
		if _, err := os.Lstat(filepath.Join(s.dir, target)); errors.Is(err, os.ErrNotExist) {
			return target, os.Rename(filepath.Join(s.dir, name), filepath.Join(s.dir, target))
		} else if err != nil {
			return "", err
		}
	}
}

// damagedFiles lists the state files set aside as damaged, by this start or
// an earlier one. While any is kept, a container or image may be missing
// from the engine's view, so nothing is deleted on the strength of that view.
func (s *State) damagedFiles() ([]string, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, entry := range entries {
		if strings.Contains(entry.Name(), corruptMark) {
			names = append(names, entry.Name())
		}
	}
	return names, nil
}

// damageSummary says in one short line which state files keep destructive
// cleanup off; New logs the full list.
func (e *Engine) damageSummary() string {
	return fmt.Sprintf("%d state file(s) in %s were not loaded (see the daemon log, or *%s* files there)", len(e.damaged), e.state.dir, corruptMark)
}

// restore loads the container records; see restoreRecords.
func (s *State) restore() (map[string]*container, error) {
	containers, _, err := s.restoreRecords()
	return containers, err
}

// restoreRecords loads every usable container record. One that does not
// parse or does not hold together is set aside (setAside) and one that
// cannot be read is left where it is; either way the other containers still
// load. skipped names the records that stayed in place unloaded.
func (s *State) restoreRecords() (containers map[string]*container, skipped []string, err error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, nil, err
	}
	containers = make(map[string]*container)
	names := make(map[string]bool)
	for _, entry := range entries {
		id, found := strings.CutSuffix(entry.Name(), ".json")
		if !found || len(id) != 64 {
			continue
		}
		if _, err := hex.DecodeString(id); err != nil {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(s.dir, entry.Name()))
		if err != nil {
			// The bytes may be fine (permissions, a passing I/O error), so
			// the file stays as it is. No container in memory has its ID,
			// so nothing writes over it.
			log.Printf("container record %s was not loaded: %v", entry.Name(), err)
			skipped = append(skipped, entry.Name())
			continue
		}
		var rec savedContainer
		if err := json.Unmarshal(raw, &rec); err != nil {
			skipped = s.setAsideRecord(skipped, entry.Name(), err)
			continue
		}
		if rec.ID != id || rec.Run.ID != id || rec.Name == "" || names[rec.Name] {
			// Of two records with one name, the second is set aside: two
			// containers cannot answer to the same name.
			skipped = s.setAsideRecord(skipped, entry.Name(), errors.New("invalid container record"))
			continue
		}
		switch rec.State {
		case "created", "exited", "starting", "running":
		default:
			skipped = s.setAsideRecord(skipped, entry.Name(), fmt.Errorf("invalid container state %q", rec.State))
			continue
		}
		renameLegacyRecord(&rec)
		if rec.SecurityOpt == nil {
			// Records written before SecurityOpt always set no-new-privileges.
			rec.Run.Flags |= guestwire.RunNoNewPrivs
		}
		names[rec.Name] = true
		c := &container{
			id: id, name: rec.Name, image: rec.Image, imageID: rec.ImageID, created: rec.Created, run: rec.Run, blobs: rec.Blobs, state: rec.State, exit: rec.Exit, oomKilled: rec.OOMKilled, volumeNames: rec.VolumeNames,
			anonymousVolumes: rec.AnonymousVolumes,
			exposedPorts:     rec.ExposedPorts, portBindings: rec.PortBindings, consoleSize: rec.ConsoleSize, logConfig: restoredLogConfig(rec.LogConfig), networkName: rec.NetworkName, networkID: rec.NetworkID, networkInterface: rec.NetworkInterface, extraNetworks: rec.ExtraNetworks, labels: rec.Labels,
			healthcheck: rec.Healthcheck, shell: rec.Shell, health: rec.Health, entrypoint: rec.Entrypoint, init: rec.Init, capabilities: rec.Capabilities, securityOpt: rec.SecurityOpt,
			restartPolicy: rec.RestartPolicy, restartCount: rec.RestartCount, manualStopped: rec.ManualStopped, links: rec.Links,
			startedAt: rec.StartedAt, finishedAt: rec.FinishedAt, vmStopped: rec.VMStopped,
		}
		c.stopSignal, c.stopTimeout = rec.StopSignal, 10
		if rec.StopTimeout != nil {
			c.stopTimeout = *rec.StopTimeout
		}
		if rec.Failure != "" {
			c.failure = errors.New(rec.Failure)
		}
		for _, entry := range rec.Logs {
			c.appendLog(record{stream: entry.Stream, data: entry.Data, at: entry.At})
		}
		if c.state == "starting" || c.state == "running" {
			c.state, c.exit = "exited", 255
			c.finishedAt, c.vmStopped = time.Now(), true
			if c.health != nil {
				c.health.Status = healthUnhealthy
			}
			c.failure = errors.New("daemon stopped before the container reported its exit")
			if err := s.save(c); err != nil {
				return nil, nil, err
			}
		}
		containers[id] = c
	}
	return containers, skipped, nil
}

// setAsideRecord moves a damaged container record aside and logs why. A
// record that cannot be moved stays in place and is added to skipped.
func (s *State) setAsideRecord(skipped []string, name string, cause error) []string {
	copy, err := s.setAside(name)
	if err != nil {
		log.Printf("container record %s is damaged (%v) and was not loaded; it could not be set aside: %v", name, cause, err)
		return append(skipped, name)
	}
	log.Printf("container record %s is damaged (%v): kept as %s and not loaded", name, cause, filepath.Join(s.dir, copy))
	return skipped
}

// VMSettings holds the next boot configuration and the explicit stop state.
type VMSettings struct {
	CPUs      uint   `json:"cpus"`
	MemoryMiB uint64 `json:"memory_mib"`
	// Network is the VM uplink mode; empty means VMNetworkUser.
	Network string `json:"network,omitempty"`
	Stopped bool   `json:"stopped,omitempty"`
}

// VM uplink modes. VMNetworkUser hands every guest connection to the
// daemon, which opens it as an ordinary macOS process, so VPNs, Tailscale
// and per-domain resolvers apply to containers. VMNetworkNAT uses the
// Virtualization framework's NAT: faster, but traffic that macOS routes
// into a VPN tunnel may not get through.
const (
	VMNetworkUser = "user"
	VMNetworkNAT  = "nat"
)

// NetworkMode is the uplink mode these settings boot with.
func (v VMSettings) NetworkMode() string {
	if v.Network == "" {
		return VMNetworkUser
	}
	return v.Network
}

// ValidVMNetwork reports whether mode names a VM uplink mode ("" is the default).
func ValidVMNetwork(mode string) bool {
	return mode == "" || mode == VMNetworkUser || mode == VMNetworkNAT
}

func (s *State) LoadVMSettings(defaults VMSettings) (VMSettings, error) {
	raw, err := os.ReadFile(filepath.Join(s.dir, "vm-settings.json"))
	if errors.Is(err, os.ErrNotExist) {
		return defaults, nil
	}
	if err != nil {
		return VMSettings{}, err
	}
	var settings VMSettings
	if err := json.Unmarshal(raw, &settings); err != nil {
		return VMSettings{}, fmt.Errorf("read VM settings: %w", err)
	}
	return settings, nil
}

func (s *State) SaveVMSettings(settings VMSettings) error {
	return saveJSON(s.dir, "vm-settings.json", settings)
}

func (s *State) restoreNetworks() (map[string]dockerNetwork, error) {
	networks := make(map[string]dockerNetwork)
	raw, err := os.ReadFile(filepath.Join(s.dir, "networks.json"))
	if errors.Is(err, os.ErrNotExist) {
		return networks, nil
	}
	if err != nil {
		return nil, err
	}
	var saved map[string]dockerNetwork
	if err := json.Unmarshal(raw, &saved); err != nil {
		return nil, fmt.Errorf("read network index: %w", err)
	}
	bridges := make(map[string]bool)
	for key, n := range saved {
		// An index saved before the rename names its bridges "kiri<id>".
		n.Bridge = renameLegacyBridge(n.Bridge)
		id, err := hex.DecodeString(n.ID)
		if key != n.Name || !networkNamePattern.MatchString(key) || key == "bridge" || key == "default" || key == "none" || key == "host" ||
			err != nil || len(id) != 32 || !guestwire.CustomBridge(n.Bridge) || n.Bridge != "fabo"+n.ID[:10] || bridges[n.Bridge] ||
			!validSavedNetworkPools(n, networks) {
			return nil, fmt.Errorf("invalid saved network %q", key)
		}
		bridges[n.Bridge] = true
		networks[key] = n
	}
	return networks, nil
}

// Before the rename to Fabo, records named the VM's bridges "kiri0" and
// "kiri<id>", kept volumes under /var/lib/kiri/volumes and host files under
// /mnt/kiri-host. The bridges are made again at each boot and both
// directories are only where the VM mounts the same disk and share, so an
// old record keeps working once it uses today's names.
const (
	legacyVolumeRoot = "/var/lib/kiri/volumes"
	legacyHostShare  = "/mnt/kiri-host"
)

// renameLegacyBridge gives a bridge name from before the rename its new
// name; any other name is returned as it is.
func renameLegacyBridge(name string) string {
	if name == "kiri0" {
		return guestwire.DefaultBridge
	}
	if rest, ok := strings.CutPrefix(name, "kiri"); ok && guestwire.CustomBridge("fabo"+rest) {
		return "fabo" + rest
	}
	return name
}

// renameLegacyMount moves a mount's source from the old VM paths to
// today's; the rest of the mount stays as it was written.
func renameLegacyMount(mount string) string {
	for old, now := range map[string]string{legacyVolumeRoot: guestwire.VolumeRoot, legacyHostShare: guestwire.HostSharePath} {
		if rest, ok := strings.CutPrefix(mount, old); ok && (rest == "" || rest[0] == '/' || rest[0] == '\t' || rest[0] == ':') {
			return now + rest
		}
	}
	return mount
}

// renameLegacyRecord applies today's names to a container record written
// before the rename.
func renameLegacyRecord(rec *savedContainer) {
	rec.Run.Network.Bridge = renameLegacyBridge(rec.Run.Network.Bridge)
	for i := range rec.Run.AdditionalNetworks {
		rec.Run.AdditionalNetworks[i].Bridge = renameLegacyBridge(rec.Run.AdditionalNetworks[i].Bridge)
	}
	for i := range rec.ExtraNetworks {
		rec.ExtraNetworks[i].Network.Bridge = renameLegacyBridge(rec.ExtraNetworks[i].Network.Bridge)
	}
	for i, mount := range rec.Run.Mounts {
		rec.Run.Mounts[i] = renameLegacyMount(mount)
	}
}

// savedLogConfig leaves a record without the default log configuration.
func savedLogConfig(l logConfig) *logConfig {
	if len(l.Config) == 0 {
		return nil
	}
	return &l
}

// restoredLogConfig is a record's log configuration, json-file by default.
func restoredLogConfig(l *logConfig) logConfig {
	if l == nil {
		return logConfig{Type: "json-file"}
	}
	return *l
}
