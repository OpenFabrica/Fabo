//go:build linux || darwin

package localengine

import "testing"

func TestWaitKeepsItsRunOutcomeAcrossRestart(t *testing.T) {
	c := &container{id: "id", state: "running"}
	e := &Engine{containers: map[string]*container{c.id: c}}
	first, err := e.prepareWait(c.id, "next-exit")
	if err != nil {
		t.Fatal(err)
	}
	e.finish(c, 41, nil)
	second, err := e.prepareWait(c.id, "next-exit")
	if err != nil {
		t.Fatal(err)
	}
	c.state = "running"
	e.finish(c, 42, nil)
	if got, err := first(t.Context()); err != nil || got != 41 {
		t.Fatalf("first outcome: %d, %v", got, err)
	}
	if got, err := second(t.Context()); err != nil || got != 42 {
		t.Fatalf("second outcome: %d, %v", got, err)
	}
}
