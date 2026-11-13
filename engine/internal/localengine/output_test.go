//go:build linux || darwin

package localengine

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestOutputSpillPreservesEveryRecordAndCleansUp(t *testing.T) {
	dir := t.TempDir()
	l := newOutputListener(dir)
	defer l.cancel()
	stamp := time.Now()
	const count = 1000
	for i := 0; i < count; i++ {
		if err := l.enqueue(record{stream: byte(i%2 + 1), data: []byte(fmt.Sprint(i)), at: stamp}); err != nil {
			t.Fatal(err)
		}
	}
	files, err := os.ReadDir(dir)
	if err != nil || len(files) != 1 {
		t.Fatalf("expected a private spill file: %v, %v", files, err)
	}
	l.finish()
	i := 0
	for entry := range l.output {
		if string(entry.data) != fmt.Sprint(i) || entry.stream != byte(i%2+1) || !entry.at.Equal(stamp) {
			t.Fatalf("record %d changed: %+v", i, entry)
		}
		i++
	}
	<-l.done
	if i != count || l.failure() != nil {
		t.Fatalf("received %d records: %v", i, l.failure())
	}
	files, err = os.ReadDir(dir)
	if err != nil || len(files) != 0 {
		t.Fatalf("spill file survived draining: %v, %v", files, err)
	}
}

func TestOutputDisconnectCancelsBlockedDrain(t *testing.T) {
	dir := t.TempDir()
	l := newOutputListener(dir)
	for i := 0; i < 100; i++ {
		if err := l.enqueue(record{stream: 1, data: []byte("queued")}); err != nil {
			t.Fatal(err)
		}
	}
	l.cancel()
	select {
	case <-l.done:
	case <-time.After(time.Second):
		t.Fatal("disconnect left the spill worker blocked")
	}
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
		t.Fatalf("disconnect left a spill file: %v, %v", entries, err)
	}
}

func TestOutputSpillFailureKeepsAnExplicitError(t *testing.T) {
	l := newOutputListener(filepath.Join(t.TempDir(), "missing"))
	defer l.cancel()
	for i := 0; i < 64; i++ {
		if err := l.enqueue(record{stream: 1, data: []byte("queued")}); err != nil {
			t.Fatal(err)
		}
	}
	if err := l.enqueue(record{stream: 1, data: []byte("cannot buffer")}); err == nil {
		t.Fatal("spill failure was hidden")
	}
	count := 0
	for range l.output {
		count++
	}
	if count != 64 || l.failure() == nil {
		t.Fatalf("spill failure lost queued records or its error: %d, %v", count, l.failure())
	}
}
