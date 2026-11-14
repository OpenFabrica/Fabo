//go:build linux || darwin

package localengine

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/OpenFabrica/FabriBay/engine/internal/guestwire"
)

func TestExportRequiresGuestCompletionMarker(t *testing.T) {
	data := bytes.Repeat([]byte("archive data"), 24_000)
	for _, complete := range []bool{true, false} {
		t.Run(fmt.Sprint(complete), func(t *testing.T) {
			c := &container{id: "id", state: "running"}
			reply := make(chan error, 1)
			e := &Engine{
				ctx: t.Context(), containers: map[string]*container{c.id: c},
				guestArchive: func(_ context.Context, _ uint32) (net.Conn, error) {
					host, guest := net.Pipe()
					go func() {
						defer guest.Close()
						reply <- replyToExport(guest, data, complete)
					}()
					return host, nil
				},
			}
			server := httptest.NewServer(e.Handler())
			defer server.Close()
			res, err := server.Client().Get(server.URL + "/v1.44/containers/id/export")
			if err != nil {
				t.Fatal(err)
			}
			defer res.Body.Close()
			body, err := io.ReadAll(res.Body)
			if guestErr := <-reply; guestErr != nil {
				t.Fatal(guestErr)
			}
			if res.StatusCode != http.StatusOK || res.Header.Get("Content-Type") != "application/x-tar" {
				t.Fatalf("unexpected response: %s %v", res.Status, res.Header)
			}
			if complete && (err != nil || !bytes.Equal(body, data)) {
				t.Fatalf("complete export: %d bytes, %v", len(body), err)
			}
			if !complete && err == nil {
				t.Fatal("incomplete export was reported as successful")
			}
		})
	}
}

func replyToExport(guest net.Conn, data []byte, complete bool) error {
	frame, err := guestwire.ReadFrame(guest, nil)
	if err != nil {
		return err
	}
	spec, err := guestwire.DecodeCopy(frame)
	if err != nil || spec.Op != guestwire.CopyExport || spec.Flags != guestwire.CopyChunked || spec.ID != "id" {
		return fmt.Errorf("unexpected export spec %+v: %v", spec, err)
	}
	if err := guestwire.WriteFrame(guest, guestwire.Frame{Kind: guestwire.CopyDoneMessage}); err != nil {
		return err
	}
	stream := guestwire.CopyChunkWriter{Target: guest}
	if _, err := stream.Write(data); err != nil {
		return err
	}
	if complete {
		return stream.Close()
	}
	return nil
}
