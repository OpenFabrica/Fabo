package sharewire

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math"
	"os"
	"reflect"
	"testing"
)

// upstreamFrames holds frames encoded by upstream kiri-share-proto (see
// testdata/gen_upstream_frames.rs.txt for the generator).
type upstreamFrames struct {
	Full  map[string]string `json:"full"`
	Short map[string]string `json:"short"`
}

func loadUpstream(t *testing.T) upstreamFrames {
	t.Helper()
	raw, err := os.ReadFile("testdata/upstream_frames.json")
	if err != nil {
		t.Fatal(err)
	}
	var f upstreamFrames
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	return f
}

func unhex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestFullFramesMatchUpstream checks that Go encodes every frame exactly as
// the Rust implementation does, and decodes the Rust bytes to the same value.
func TestFullFramesMatchUpstream(t *testing.T) {
	want := map[string]Msg{
		"hello": Hello{Protocol: Protocol, Token: "ft_tok", Device: "0123456789abcdef0123456789abcdef", Name: "web",
			Target: "web (localhost:8080)", Client: "fabo 0.3.0", Features: FeatureWindow, Machine: "3f9a0c41d2e87b65aabbccdd", Host: "paul-mbp"},
		"welcome": Welcome{URL: "https://web-x7k2ab.share.test", Remaining: math.MaxUint64},
		"welcome_window": Welcome{URL: "https://web.share.test", Rate: 524288, Remaining: 1 << 33, Warning: true,
			Domains: "https://a.example.com,https://b.example.org", Window: 4096},
		"refused":       Refused{Reason: "web is already in use"},
		"open":          Open{Stream: 7, Remote: "203.0.113.9:51234"},
		"data":          Data{Stream: 7, Data: []byte("GET / HTTP/1.1\r\n\r\n")},
		"data_empty":    Data{Stream: 7, Data: []byte{}},
		"window":        Window{Stream: 7, Credit: 65536},
		"close":         Close{Stream: 7},
		"reset":         Reset{Stream: math.MaxUint32},
		"ping":          Ping{Value: 0x0102030405060708},
		"pong":          Pong{Value: 42},
		"notice":        Notice{Text: "nothing is answering on localhost:3000"},
		"goodbye":       Goodbye{Reconnect: true, Reason: "edge restarting"},
		"goodbye_final": Goodbye{Reason: "stopped from paul-mbp"},
	}
	up := loadUpstream(t)
	if len(up.Full) != len(want) {
		t.Fatalf("fixture has %d frames, test knows %d", len(up.Full), len(want))
	}
	for name, msg := range want {
		rust := unhex(t, up.Full[name])
		if got := Append(nil, msg); !bytes.Equal(got, rust) {
			t.Errorf("%s: Go encodes\n%x\nRust encodes\n%x", name, got, rust)
		}
		decoded, err := Decode(rust)
		if err != nil {
			t.Errorf("%s: decode: %v", name, err)
			continue
		}
		if d, ok := decoded.(Data); ok && len(d.Data) == 0 {
			d.Data = []byte{}
			decoded = d
		}
		if !reflect.DeepEqual(decoded, msg) {
			t.Errorf("%s: decoded %#v, want %#v", name, decoded, msg)
		}
	}
}

// TestShortFramesFromOlderPeersDecode checks optional trailing fields read
// as zero values; re-encoding is not expected to reproduce the short form.
func TestShortFramesFromOlderPeersDecode(t *testing.T) {
	old := Hello{Protocol: 1, Token: "tok", Device: "dev", Name: "web", Target: "localhost:3000", Client: "kiri 0.0.9"}
	withFeatures := old
	withFeatures.Features = FeatureWindow
	oldWelcome := Welcome{URL: "https://web.share.test", Remaining: math.MaxUint64}
	withDomains := oldWelcome
	withDomains.Domains = "https://dev.example.com"
	want := map[string]Msg{
		"hello_old":            old,
		"hello_features_only":  withFeatures,
		"welcome_old":          oldWelcome,
		"welcome_domains_only": withDomains,
	}
	up := loadUpstream(t)
	for name, msg := range want {
		got, err := Decode(unhex(t, up.Short[name]))
		if err != nil || !reflect.DeepEqual(got, msg) {
			t.Errorf("%s: decoded %#v, %v; want %#v", name, got, err, msg)
		}
	}
	if w := want["welcome_old"].(Welcome); w.SendWindow() != InitialWindow {
		t.Fatalf("missing window should mean InitialWindow, got %d", w.SendWindow())
	}
}

func TestReadFrameRoundTrip(t *testing.T) {
	var wire []byte
	msgs := []Msg{Ping{Value: 1}, Data{Stream: 3, Data: bytes.Repeat([]byte("x"), MaxChunk)}, Goodbye{Reason: "bye"}}
	for _, m := range msgs {
		wire = Append(wire, m)
	}
	r := bytes.NewReader(wire)
	var buf []byte
	for _, want := range msgs {
		frame, err := ReadFrame(r, buf)
		if err != nil {
			t.Fatal(err)
		}
		got, err := Decode(frame)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("got %T %v, want %T", got, err, want)
		}
		buf = frame
	}
	if _, err := ReadFrame(r, buf); !errors.Is(err, io.EOF) {
		t.Fatalf("end of stream reported %v", err)
	}
}

func TestRejectsOversizedUnknownReservedAndTruncated(t *testing.T) {
	if _, err := FrameLen([4]byte{0, 0, 1, 0}); err == nil {
		t.Fatal("a MaxFrame-sized length must be rejected (header makes it larger)")
	}
	if _, err := FrameLen([4]byte{1, 0, 0, 0}); err == nil {
		t.Fatal("a frame without a kind must be rejected")
	}
	if _, err := Decode([]byte{2, 0, 0, 0, 99, 0}); err == nil || errors.Is(err, ErrUnsupported) {
		t.Fatalf("unknown kind: %v", err)
	}
	for kind := byte(13); kind <= 17; kind++ {
		if _, err := Decode([]byte{2, 0, 0, 0, kind, 0}); !errors.Is(err, ErrUnsupported) {
			t.Fatalf("reserved kind %d: %v", kind, err)
		}
	}
	if _, err := Decode([]byte{6, 0, 0, 0, 6, 0, 1, 0}); !errors.Is(err, ErrTruncated) {
		t.Fatalf("short Window: %v", err)
	}
	if _, err := Decode([]byte{1, 0}); !errors.Is(err, ErrTruncated) {
		t.Fatalf("short header: %v", err)
	}
	// A string length past the end of the frame.
	bad := Append(nil, Hello{Token: "x"})
	bad[8] = 0xff
	if _, err := Decode(bad); !errors.Is(err, ErrTruncated) {
		t.Fatalf("overlong string: %v", err)
	}
	if _, err := Decode(Append(nil, Notice{Text: "\xff"})); !errors.Is(err, ErrBadUTF8) {
		t.Fatalf("bad UTF-8: %v", err)
	}
	var truncated bytes.Buffer
	truncated.Write(Append(nil, Ping{Value: 9})[:8])
	if _, err := ReadFrame(&truncated, nil); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("truncated stream: %v", err)
	}
}
