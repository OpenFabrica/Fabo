//go:build linux || darwin

package localengine

import (
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/libracoder/kiri/engine/internal/guestwire"
)

func TestVolumeReferenceSurvivesStateRestore(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	store, err := OpenState(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	e := &Engine{state: store, images: map[string]Image{"local": {Cmd: []string{"/bin/test"}}}, containers: map[string]*container{}}
	id, err := e.create("test", Config{Image: "local", volumes: []volumeMount{{name: "volume-one", target: "/data"}}})
	if err != nil {
		t.Fatal(err)
	}
	v, err := e.readVolume("volume-one")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(v.Mountpoint, "data"), []byte("kept"), 0600); err != nil {
		t.Fatal(err)
	}
	e.containers, err = store.restore()
	if err != nil {
		t.Fatal(err)
	}
	if err := e.removeVolume(t.Context(), "volume-one"); !errors.Is(err, errConflict) {
		t.Fatalf("removing referenced volume: %v", err)
	}
	if err := e.deleteRecord(e.containers[id]); err != nil {
		t.Fatal(err)
	}
	if raw, err := os.ReadFile(filepath.Join(v.Mountpoint, "data")); err != nil || string(raw) != "kept" {
		t.Fatalf("removing container lost volume data: %q %v", raw, err)
	}
	if err := e.removeVolume(t.Context(), "volume-one"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(v.Mountpoint); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("volume directory remains: %v", err)
	}
}

func TestAnonymousVolumeOwnershipSurvivesStateRestore(t *testing.T) {
	state := &State{dir: t.TempDir()}
	e := &Engine{state: state, images: map[string]Image{"local": {Cmd: []string{"/bin/test"}}}, containers: map[string]*container{}}
	id, err := e.create("owner", Config{Image: "local", volumes: []volumeMount{
		{target: "/private-data"}, {target: "/shared-data"}, {name: "named", target: "/named"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	owned := e.containers[id].anonymousVolumes
	if len(owned) != 2 {
		t.Fatalf("anonymous volumes: %v", owned)
	}
	keeper, err := e.create("keeper", Config{Image: "local", volumes: []volumeMount{{name: owned[1], target: "/saved"}}})
	if err != nil {
		t.Fatal(err)
	}
	e.containers, err = state.restore()
	if err != nil {
		t.Fatal(err)
	}
	owner := e.containers[id]
	if !slices.Equal(owner.anonymousVolumes, owned) || len(e.containers[keeper].anonymousVolumes) != 0 {
		t.Fatalf("restored anonymous ownership: %v, %v", owner.anonymousVolumes, e.containers[keeper].anonymousVolumes)
	}
	if err := e.deleteRecord(owner); err != nil {
		t.Fatal(err)
	}
	if err := e.removeAnonymousVolumes(t.Context(), owner.anonymousVolumes); err != nil {
		t.Fatal(err)
	}
	if _, err := e.readVolume(owned[0]); !errors.Is(err, errMissing) {
		t.Fatalf("unreferenced anonymous volume: %v", err)
	}
	for _, name := range []string{owned[1], "named"} {
		if _, err := e.readVolume(name); err != nil {
			t.Fatalf("volume %q should remain: %v", name, err)
		}
	}
}

// A guest deletion failure must keep host metadata, and the control
// reader must still process other containers' events while waiting.
func TestVMVolumeRemovalWaitsForGuest(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	state, err := OpenState(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	c := &container{id: "live", state: "running"}
	e := &Engine{state: state, remoteGuest: true, guestDisk: true, containers: map[string]*container{c.id: c}}
	if _, err := e.ensureVolume("vm-volume", nil, nil); err != nil {
		t.Fatal(err)
	}
	host, guest := net.Pipe()
	defer guest.Close()
	_ = guest.SetDeadline(time.Now().Add(3 * time.Second))
	e.session = guestwire.NewSession(host, e.onEvent)
	defer e.session.Close()

	remove := func(reply guestwire.Frame) error {
		t.Helper()
		result := make(chan error, 1)
		go func() { result <- e.removeVolume(t.Context(), "vm-volume") }()
		frame, err := guestwire.ReadFrame(guest, nil)
		if err != nil {
			t.Fatal(err)
		}
		if name, err := guestwire.DecodeVolumeName(frame); err != nil || frame.Kind != guestwire.DeleteVolumeMessage || name != "vm-volume" {
			t.Fatalf("unexpected deletion request: %v %q %v", frame.Kind, name, err)
		}
		output, err := (guestwire.RunEvent{Kind: guestwire.OutputMessage, ID: c.id, Stream: 1, Data: []byte("alive")}).Frame()
		if err != nil {
			t.Fatal(err)
		}
		if err := guestwire.WriteFrame(guest, output); err != nil {
			t.Fatal(err)
		}
		if err := guestwire.WriteFrame(guest, reply); err != nil {
			t.Fatal(err)
		}
		select {
		case err := <-result:
			return err
		case <-time.After(3 * time.Second):
			t.Fatal("volume removal did not receive its reply")
			return nil
		}
	}
	if err := remove(guestwire.Frame{Kind: guestwire.ErrorMessage, Payload: []byte("disk unavailable")}); err == nil {
		t.Fatal("guest deletion failure was ignored")
	}
	if _, err := e.readVolume("vm-volume"); err != nil {
		t.Fatalf("metadata disappeared before successful removal: %v", err)
	}
	if err := remove(guestwire.Frame{Kind: guestwire.AckMessage}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.readVolume("vm-volume"); !errors.Is(err, errMissing) {
		t.Fatalf("metadata remains after deletion: %v", err)
	}
}

func TestVolumePruneUsesStoredAnonymityAndReferences(t *testing.T) {
	e := &Engine{state: &State{dir: t.TempDir()}, containers: map[string]*container{}}
	create := func(name string, labels map[string]string) volume {
		t.Helper()
		v, err := e.ensureVolume(name, labels, nil)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	selected := create("", map[string]string{"remove": "yes"})
	referenced := create("", map[string]string{"remove": "yes"})
	protected := create("", map[string]string{"remove": "yes", "protect": "yes"})
	named := create(strings.Repeat("a", 64), map[string]string{"remove": "yes"})
	e.containers["stopped"] = &container{state: "exited", volumeNames: []string{referenced.Name}}

	// Recreate the engine around its persisted volume metadata.
	e = &Engine{state: e.state, containers: e.containers}
	prune := func(filters string) []string {
		t.Helper()
		w := httptest.NewRecorder()
		e.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1.44/volumes/prune?filters="+url.QueryEscape(filters), nil))
		var result struct{ VolumesDeleted []string }
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || w.Code != http.StatusOK {
			t.Fatalf("prune HTTP %d: %s", w.Code, w.Body)
		}
		return result.VolumesDeleted
	}
	filter := `{"label":["remove=yes"],"label!":["protect=yes"]}`
	if deleted := prune(filter); !slices.Equal(deleted, []string{selected.Name}) {
		t.Fatalf("default prune removed %v", deleted)
	}
	filter = `{"all":["true"],"label":["remove=yes"],"label!":["protect=yes"]}`
	if deleted := prune(filter); !slices.Equal(deleted, []string{named.Name}) {
		t.Fatalf("all-volumes prune removed %v", deleted)
	}
	for _, name := range []string{referenced.Name, protected.Name} {
		if _, err := e.readVolume(name); err != nil {
			t.Fatalf("kept volume %q: %v", name, err)
		}
	}
}
