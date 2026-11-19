//go:build linux || darwin

package localengine

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/OpenFabrica/Fabo/engine/internal/guestwire"
	"github.com/OpenFabrica/Fabo/engine/internal/hostagent"
)

// damageFixture is a state directory with host blobs and the state handle.
type damageFixture struct {
	dir   string
	state *State
}

// newDamageFixture opens a private state directory for a damage test.
func newDamageFixture(t *testing.T) *damageFixture {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	state, err := OpenState(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { state.Close() })
	return &damageFixture{dir: dir, state: state}
}

// blob writes a host layer blob named after the digest of seed and returns its layer.
func (f *damageFixture) blob(t *testing.T, seed string) hostagent.Layer {
	t.Helper()
	sum := sha256.Sum256([]byte(seed))
	digest := hex.EncodeToString(sum[:])
	if err := os.MkdirAll(filepath.Join(f.dir, "blobs"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.dir, "blobs", digest), []byte(seed), 0600); err != nil {
		t.Fatal(err)
	}
	return hostagent.Layer{Digest: "sha256:" + digest}
}

// hasBlob reports whether the host blob of layer is still on disk.
func (f *damageFixture) hasBlob(layer hostagent.Layer) bool {
	_, err := os.Stat(filepath.Join(f.dir, "blobs", strings.TrimPrefix(layer.Digest, "sha256:")))
	return err == nil
}

// copies returns the set-aside copies of name in the state directory.
func (f *damageFixture) copies(t *testing.T, name string) []string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(f.dir, name+corruptMark+"*"))
	if err != nil {
		t.Fatal(err)
	}
	return matches
}

// TestDamagedContainerRecordIsSetAsideAndProtectsLayers checks a container
// record that does not parse no longer stops New: it is kept as a
// .corrupt- copy, the other containers and images load, and while the copy
// exists neither layer cleanup nor volume removal deletes what it may use.
func TestDamagedContainerRecordIsSetAsideAndProtectsLayers(t *testing.T) {
	f := newDamageFixture(t)
	shared, goodOnly, badOnly, imageOnly, unused := f.blob(t, "shared"), f.blob(t, "good"), f.blob(t, "bad"), f.blob(t, "image"), f.blob(t, "unused")
	seed := &Engine{state: f.state, containers: map[string]*container{}, images: map[string]Image{
		"good": {Cmd: []string{"/bin/good"}, Layers: []hostagent.Layer{shared, goodOnly}},
		"bad":  {Cmd: []string{"/bin/bad"}, Layers: []hostagent.Layer{shared, badOnly}},
	}}
	good, err := seed.create("good", Config{Image: "good"})
	if err != nil {
		t.Fatal(err)
	}
	bad, err := seed.create("bad", Config{Image: "bad", volumes: []volumeMount{{name: "bad-data", target: "/data"}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := saveJSON(f.dir, "image-index.json", imageIndex{Overrides: map[string]*Image{"kept:latest": {Cmd: []string{"/bin/kept"}, Layers: []hostagent.Layer{imageOnly}}}}); err != nil {
		t.Fatal(err)
	}
	// A record cut short, as a failing disk leaves it.
	broken := []byte(`{"ID":"` + bad + `","Name":"bad","Blobs":[{"Dig`)
	if err := os.WriteFile(filepath.Join(f.dir, bad+".json"), broken, 0600); err != nil {
		t.Fatal(err)
	}

	e, err := New(t.Context(), nil, nil, f.state, VMOptions{})
	if err != nil {
		t.Fatalf("New with a damaged record: %v", err)
	}
	if e.containers[good] == nil || e.containers[bad] != nil || len(e.containers) != 1 {
		t.Fatalf("restored containers: %v", e.containers)
	}
	if _, ok := e.images["kept:latest"]; !ok {
		t.Fatalf("images: %v", e.images)
	}
	if _, err := os.Stat(filepath.Join(f.dir, bad+".json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("damaged record left in place: %v", err)
	}
	copies := f.copies(t, bad+".json")
	if len(copies) != 1 {
		t.Fatalf("copies: %v", copies)
	}
	if raw, err := os.ReadFile(copies[0]); err != nil || string(raw) != string(broken) {
		t.Fatalf("copy holds %q, %v", raw, err)
	}
	if reclaimed, err := e.collectUnusedLayers(); err != nil || reclaimed != 0 {
		t.Fatalf("layer cleanup: %d %v", reclaimed, err)
	}
	for _, layer := range []hostagent.Layer{shared, goodOnly, badOnly, imageOnly, unused} {
		if !f.hasBlob(layer) {
			t.Fatalf("layer %s deleted while a record was set aside", layer.Digest)
		}
	}
	// The set-aside container's volume is not taken for unused. (Without a
	// guest the engine is closed and stores volumes on the VM disk; check
	// the reference logic the way a host engine would run it.)
	e.remoteGuest, e.closed = false, false
	if err := e.removeVolume(t.Context(), "bad-data"); !errors.Is(err, errConflict) {
		t.Fatalf("removing a volume a set-aside container may use: %v", err)
	}

	// The VM sleeps and wakes: New runs again and must still hold back.
	e, err = New(t.Context(), nil, nil, f.state, VMOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.collectUnusedLayers(); err != nil || !f.hasBlob(badOnly) || !f.hasBlob(unused) {
		t.Fatalf("second start reclaimed layers: %v", err)
	}
	if len(f.copies(t, bad+".json")) != 1 {
		t.Fatal("a second start set the copy aside again")
	}

	// Once the copy is dealt with, cleanup works as before.
	if err := os.Remove(copies[0]); err != nil {
		t.Fatal(err)
	}
	e, err = New(t.Context(), nil, nil, f.state, VMOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.collectUnusedLayers(); err != nil {
		t.Fatal(err)
	}
	if !f.hasBlob(shared) || !f.hasBlob(goodOnly) || !f.hasBlob(imageOnly) || f.hasBlob(badOnly) || f.hasBlob(unused) {
		t.Fatal("cleanup after the copy was removed kept the wrong layers")
	}
}

// TestInconsistentContainerRecordsAreSetAside checks records that parse but
// do not hold together are set aside one by one instead of failing New.
func TestInconsistentContainerRecordsAreSetAside(t *testing.T) {
	f := newDamageFixture(t)
	seed := &Engine{state: f.state, containers: map[string]*container{}, images: map[string]Image{"local": {Cmd: []string{"/bin/test"}}}}
	var ids []string
	for _, name := range []string{"one", "two", "three"} {
		id, err := seed.create(name, Config{Image: "local"})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	// An unknown state, and a second record claiming a taken name.
	seed.containers[ids[0]].state = "frozen"
	if err := f.state.save(seed.containers[ids[0]]); err != nil {
		t.Fatal(err)
	}
	seed.containers[ids[1]].name = seed.containers[ids[2]].name
	if err := f.state.save(seed.containers[ids[1]]); err != nil {
		t.Fatal(err)
	}
	e, err := New(t.Context(), nil, nil, f.state, VMOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(e.containers) != 1 || len(e.damaged) != 2 {
		t.Fatalf("containers %d, damaged %v", len(e.containers), e.damaged)
	}
	if len(f.copies(t, ids[0]+".json")) != 1 {
		t.Fatal("record with an unknown state was not set aside")
	}
}

// TestDamagedImageIndexIsSetAside checks an image index that does not parse
// is kept as a copy and replaced by an empty one, so the next start neither
// fails nor falls back to the legacy images.json, and no layer is reclaimed.
func TestDamagedImageIndexIsSetAside(t *testing.T) {
	f := newDamageFixture(t)
	layer := f.blob(t, "only in the lost index")
	seed := &Engine{state: f.state, containers: map[string]*container{}, images: map[string]Image{"local": {Cmd: []string{"/bin/test"}}}}
	id, err := seed.create("web", Config{Image: "local"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.dir, "image-index.json"), []byte(`{"Overrides":{"app:1":{"Layers":[{"Dig`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.dir, "images.json"), []byte(`{"legacy:1":{"Cmd":["/bin/old"]}}`), 0600); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		e, err := New(t.Context(), nil, nil, f.state, VMOptions{})
		if err != nil {
			t.Fatalf("New with a damaged image index: %v", err)
		}
		if e.containers[id] == nil {
			t.Fatal("container lost with the image index")
		}
		if len(e.images) != 0 {
			t.Fatalf("images after a damaged index: %v", e.images)
		}
		if _, err := e.collectUnusedLayers(); err != nil || !f.hasBlob(layer) {
			t.Fatalf("layer of the lost index reclaimed: %v", err)
		}
	}
	if len(f.copies(t, "image-index.json")) != 1 {
		t.Fatal("damaged index not kept exactly once")
	}
	if _, err := os.Stat(filepath.Join(f.dir, "image-index.json")); err != nil {
		t.Fatalf("no fresh index written: %v", err)
	}
}

// TestContainerWithStaleNetworkStillLoads checks a container whose network
// is gone from networks.json no longer stops New: it loads and refuses to
// start, saying why. A damaged networks.json is set aside the same way.
func TestContainerWithStaleNetworkStillLoads(t *testing.T) {
	f := newDamageFixture(t)
	seed := &Engine{state: f.state, containers: map[string]*container{}, images: map[string]Image{"local": {Cmd: []string{"/bin/test"}}}}
	id, err := seed.create("web", Config{Image: "local"})
	if err != nil {
		t.Fatal(err)
	}
	c := seed.containers[id]
	networkID := strings.Repeat("ab", 32)
	c.setEndpoints([]containerEndpoint{{Name: "gone", ID: networkID, Interface: "eth0", Network: guestwire.Network{Bridge: "fabo" + networkID[:10], IP: [4]byte{10, 99, 0, 2}, Prefix: 24, Gateway: [4]byte{10, 99, 0, 1}}}})
	if err := f.state.save(c); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.dir, "networks.json"), []byte(`{"gone":`), 0600); err != nil {
		t.Fatal(err)
	}
	e, err := New(t.Context(), nil, nil, f.state, VMOptions{})
	if err != nil {
		t.Fatalf("New with a stale network reference: %v", err)
	}
	if e.containers[id] == nil {
		t.Fatal("container with a stale network was not loaded")
	}
	if err := e.endpointProblem(e.containers[id]); !errors.Is(err, errConflict) || !strings.Contains(err.Error(), "gone") {
		t.Fatalf("start check: %v", err)
	}
	// Start refuses it before asking the guest, and records why.
	e.closed = false
	if err := e.start(id); !errors.Is(err, errConflict) {
		t.Fatalf("start: %v", err)
	}
	if c := e.containers[id]; c.state != "exited" || c.failure == nil || !strings.Contains(c.failure.Error(), "gone") {
		t.Fatalf("after a refused start: %s %v", c.state, c.failure)
	}
	if len(f.copies(t, "networks.json")) != 1 {
		t.Fatal("damaged networks.json not kept")
	}
}

// TestUnreadableContainerRecordStaysAndProtectsLayers checks a record that
// cannot be read is left where it is, unloaded, and keeps layer cleanup off
// on every start although no .corrupt- copy exists.
func TestUnreadableContainerRecordStaysAndProtectsLayers(t *testing.T) {
	f := newDamageFixture(t)
	layer := f.blob(t, "maybe used by the unreadable record")
	// A directory where a record should be: reading it fails, as an I/O
	// error or a permission problem would, also when run as root.
	name := strings.Repeat("cd", 32) + ".json"
	if err := os.Mkdir(filepath.Join(f.dir, name), 0700); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		e, err := New(t.Context(), nil, nil, f.state, VMOptions{})
		if err != nil {
			t.Fatalf("New with an unreadable record: %v", err)
		}
		if len(e.damaged) != 1 || e.damaged[0] != name {
			t.Fatalf("damaged: %v", e.damaged)
		}
		if _, err := e.collectUnusedLayers(); err != nil || !f.hasBlob(layer) {
			t.Fatalf("layer reclaimed beside an unreadable record: %v", err)
		}
	}
	if info, err := os.Stat(filepath.Join(f.dir, name)); err != nil || !info.IsDir() {
		t.Fatalf("unreadable record moved: %v", err)
	}
	if copies := f.copies(t, name); len(copies) != 0 {
		t.Fatalf("unreadable record set aside: %v", copies)
	}
}
