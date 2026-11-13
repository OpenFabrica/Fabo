//go:build linux || darwin

package localengine

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
)

const eventHistorySize = 256

// EventBus keeps local daemon events across VM stops and starts.
// Its zero value is ready for use.
type EventBus struct {
	mu      sync.Mutex
	history [eventHistorySize]engineEvent
	next    uint64
	changed chan struct{}
}

type eventActor struct {
	ID         string
	Attributes map[string]string
}

type engineEvent struct {
	Type     string
	Action   string
	Actor    eventActor
	Scope    string `json:"scope"`
	Time     int64  `json:"time"`
	TimeNano int64  `json:"timeNano"`
	Status   string `json:"status,omitempty"`
	ID       string `json:"id,omitempty"`
	From     string `json:"from,omitempty"`

	labels map[string]string
}

func (e *Engine) eventBus() *EventBus {
	if e.sharedEvents != nil {
		return e.sharedEvents
	}
	return &e.events
}

func (e *Engine) emitEvent(kind, action, id string, labels, attributes map[string]string) {
	fields := make(map[string]string, len(labels)+len(attributes))
	maps.Copy(fields, labels)
	maps.Copy(fields, attributes)
	event := engineEvent{
		Type: kind, Action: action, Actor: eventActor{ID: id, Attributes: fields},
		Scope: "local", labels: maps.Clone(labels),
	}
	if kind == "container" {
		event.Status, event.ID, event.From = action, id, fields["image"]
	}
	e.eventBus().publish(event)
}

func (e *Engine) emitContainer(c *container, action string, attributes map[string]string) {
	if attributes == nil {
		attributes = make(map[string]string, 2)
	}
	attributes["name"], attributes["image"] = c.name, c.image
	e.emitEvent("container", action, c.id, c.labels, attributes)
}

func (e *Engine) emitImage(image Image, action, ref string) {
	e.emitEvent("image", action, identifyImage(image).ID, image.Labels, map[string]string{"name": ref})
}

func (e *Engine) emitNetwork(n dockerNetwork, action, container string) {
	e.emitEvent("network", action, n.ID, n.Labels, map[string]string{
		"name": n.Name, "type": "bridge", "container": container,
	})
}

func (e *Engine) emitVolume(v volume, action string) {
	e.emitEvent("volume", action, v.Name, v.Labels, map[string]string{"driver": v.Driver})
}

func (b *EventBus) publish(event engineEvent) {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := time.Now()
	event.Time, event.TimeNano = now.Unix(), now.UnixNano()
	b.history[b.next%eventHistorySize] = event
	b.next++
	if b.changed != nil {
		close(b.changed)
		b.changed = nil
	}
}

func (b *EventBus) oldest() uint64 {
	return max(b.next, eventHistorySize) - eventHistorySize
}

func (b *EventBus) subscribe(replay bool) ([]engineEvent, uint64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if replay {
		return b.snapshot(b.oldest()), b.next
	}
	return nil, b.next
}

// snapshot requires b.mu.
func (b *EventBus) snapshot(cursor uint64) []engineEvent {
	events := make([]engineEvent, 0, b.next-cursor)
	for i := cursor; i < b.next; i++ {
		events = append(events, b.history[i%eventHistorySize])
	}
	return events
}

// read returns one ordered snapshot or a signal to wait on. Both share the
// same cursor so an event published during replay is read on the next pass.
func (b *EventBus) read(cursor uint64) ([]engineEvent, uint64, <-chan struct{}, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if cursor < b.oldest() {
		return nil, cursor, nil, true
	}
	if cursor == b.next {
		if b.changed == nil {
			b.changed = make(chan struct{})
		}
		return nil, cursor, b.changed, false
	}
	return b.snapshot(cursor), b.next, nil, false
}

func (e *Engine) eventsHTTP(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	filters, err := parseFilters(q.Get("filters"), "type", "event", "container", "image", "network", "volume", "label", "label!", "daemon", "scope")
	if err != nil {
		writeError(w, err)
		return
	}
	since, err := eventTime(q.Get("since"))
	if err != nil {
		writeError(w, fmt.Errorf("%w: invalid since timestamp", errInvalid))
		return
	}
	until, err := eventTime(q.Get("until"))
	if err != nil {
		writeError(w, fmt.Errorf("%w: invalid until timestamp", errInvalid))
		return
	}
	b := e.eventBus()
	batch, cursor := b.subscribe(!since.IsZero())
	w.Header().Set("Content-Type", "application/json")
	ctrl := http.NewResponseController(w)
	_ = ctrl.SetWriteDeadline(time.Now().Add(15 * time.Second))
	if err := ctrl.Flush(); err != nil || r.Method == http.MethodHead {
		return
	}
	_ = ctrl.SetWriteDeadline(time.Time{})
	var deadline <-chan time.Time
	if !until.IsZero() {
		timer := time.NewTimer(time.Until(until))
		defer timer.Stop()
		deadline = timer.C
	}
	enc := json.NewEncoder(w)
	final := false
	for {
		for _, event := range batch {
			at := time.Unix(0, event.TimeNano)
			if !until.IsZero() && at.After(until) {
				continue
			}
			if at.Before(since) || !event.matches(filters) {
				continue
			}
			_ = ctrl.SetWriteDeadline(time.Now().Add(15 * time.Second))
			if err := enc.Encode(event); err != nil {
				return
			}
			if err := ctrl.Flush(); err != nil {
				return
			}
			_ = ctrl.SetWriteDeadline(time.Time{})
		}
		if final || r.Context().Err() != nil {
			return
		}
		// Once the deadline passes, take one final snapshot to include
		// events published before it while the last batch was being written.
		final = !until.IsZero() && !time.Now().Before(until)
		var changed <-chan struct{}
		var overflow bool
		batch, cursor, changed, overflow = b.read(cursor)
		if overflow {
			panic(http.ErrAbortHandler)
		}
		if changed != nil {
			if final {
				return
			}
			select {
			case <-r.Context().Done():
				return
			case <-deadline:
			case <-changed:
			}
		}
	}
}

func eventTime(raw string) (time.Time, error) {
	if strings.TrimSpace(raw) == "0" {
		return time.Unix(0, 0), nil
	}
	return parseLogTime(raw)
}

func (e engineEvent) matches(f map[string][]string) bool {
	attrs := e.Actor.Attributes
	action, _, _ := strings.Cut(e.Action, ":")
	object := func(kind string, key string) bool {
		return e.Type == kind && (strings.HasPrefix(e.Actor.ID, key) || attrs["name"] == strings.TrimPrefix(key, "/"))
	}
	return matchAny(f["type"], func(v string) bool { return e.Type == v }) &&
		matchAny(f["event"], func(v string) bool { return e.Action == v || action == v }) &&
		matchAny(f["container"], func(v string) bool { return object("container", v) }) &&
		matchAny(f["image"], func(v string) bool {
			if e.Type == "image" {
				return strings.HasPrefix(e.Actor.ID, v) || eventImageMatches(attrs["name"], v)
			}
			return attrs["image"] != "" && eventImageMatches(attrs["image"], v)
		}) &&
		matchAny(f["network"], func(v string) bool { return object("network", v) }) &&
		matchAny(f["volume"], func(v string) bool { return e.Type == "volume" && e.Actor.ID == v }) &&
		matchAny(f["daemon"], func(v string) bool { return object("daemon", v) }) &&
		matchAny(f["scope"], func(v string) bool { return e.Scope == v }) &&
		matchesLabels(e.labels, f["label"]) &&
		!slices.ContainsFunc(f["label!"], func(v string) bool { return matchesLabels(e.labels, []string{v}) })
}

func eventImageMatches(ref, filter string) bool {
	if imageReferencesMatch([]string{ref}, []string{filter}) {
		return true
	}
	actual, err := name.ParseReference(ref)
	if err != nil {
		return false
	}
	want, err := name.ParseReference(filter)
	return err == nil && actual.Name() == want.Name()
}
