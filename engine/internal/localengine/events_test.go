//go:build linux || darwin

package localengine

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"testing"
	"time"
)

func TestEventReplayAndLiveShareCursor(t *testing.T) {
	bus := &EventBus{}
	before := &Engine{sharedEvents: bus}
	after := &Engine{sharedEvents: bus}
	c := &container{id: "abcdef", name: "worker", image: "busybox:latest", labels: map[string]string{"team": "infra"}}
	for i := range eventHistorySize + 2 {
		before.emitContainer(c, "create", map[string]string{"seq": strconv.Itoa(i)})
	}
	server := httptest.NewServer(before.Handler())
	defer server.Close()
	client := server.Client()
	client.Timeout = 5 * time.Second
	params := url.Values{"since": {"0"}, "filters": {`{"event":["create","die"],"label":["team=infra"],"container":["/worker"],"type":["container"]}`}}
	res, err := client.Get(server.URL + "/v1.44/events?" + params.Encode())
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK || !slices.Contains(res.TransferEncoding, "chunked") {
		t.Fatalf("unexpected stream response: %s %v", res.Status, res.TransferEncoding)
	}
	// The response belongs to the old guest handler, while the new guest
	// can publish before the client has consumed the historical events.
	after.emitContainer(c, "die", map[string]string{"exitCode": "7"})
	dec := json.NewDecoder(res.Body)
	for i := 2; i < eventHistorySize+2; i++ {
		var event engineEvent
		if err := dec.Decode(&event); err != nil {
			t.Fatal(err)
		}
		if event.Action != "create" || event.Actor.Attributes["seq"] != strconv.Itoa(i) {
			t.Fatalf("event %d was skipped, repeated or reordered: %+v", i, event)
		}
	}
	var event engineEvent
	if err := dec.Decode(&event); err != nil {
		t.Fatal(err)
	}
	if event.Action != "die" || event.Status != "die" || event.Actor.ID != c.id || event.ID != c.id ||
		event.From != c.image || event.Scope != "local" || event.Actor.Attributes["exitCode"] != "7" ||
		event.TimeNano/1e9 != event.Time {
		t.Fatalf("unexpected live event: %+v", event)
	}
}

func TestEventStreamWithoutSinceSkipsHistory(t *testing.T) {
	e := &Engine{}
	c := &container{id: "a", name: "worker", image: "busybox:latest", labels: map[string]string{"team": "infra"}}
	e.emitContainer(c, "create", nil)
	server := httptest.NewServer(e.Handler())
	defer server.Close()
	client := server.Client()
	client.Timeout = 5 * time.Second
	params := url.Values{"filters": {`{"event":{"exec_start":true,"die":true},"label!":["team=other"],"image":["busybox"]}`}}
	res, err := client.Get(server.URL + "/events?" + params.Encode())
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	e.emitContainer(c, "pause", nil)
	e.emitContainer(c, "exec_start: true", nil)
	var event engineEvent
	if err := json.NewDecoder(res.Body).Decode(&event); err != nil || event.Action != "exec_start: true" {
		t.Fatalf("unexpected live event: %+v, %v", event, err)
	}
}

func TestEventHistoryHonorsNanosecondBounds(t *testing.T) {
	e := &Engine{}
	at := time.Unix(1, 345)
	e.events.history[0] = engineEvent{Action: "create", TimeNano: at.UnixNano()}
	e.events.history[1] = engineEvent{Action: "start", TimeNano: at.UnixNano() + 1}
	e.events.next = 2
	params := url.Values{"since": {"1.000000345"}, "until": {at.Format(time.RFC3339Nano)}}
	req := httptest.NewRequest(http.MethodGet, "/events?"+params.Encode(), nil)
	w := httptest.NewRecorder()
	e.Handler().ServeHTTP(w, req)
	dec := json.NewDecoder(w.Body)
	var event engineEvent
	if err := dec.Decode(&event); err != nil || event.Action != "create" {
		t.Fatalf("unexpected historical event: %+v, %v", event, err)
	}
	if err := dec.Decode(&event); !errors.Is(err, io.EOF) {
		t.Fatalf("expected history to end at the requested bound, got %v", err)
	}
}
