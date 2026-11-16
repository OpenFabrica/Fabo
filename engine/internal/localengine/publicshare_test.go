//go:build linux || darwin

package localengine

import (
	"encoding/json"
	"fmt"
	"testing"
)

// The account view is a snapshot: encoding it after the lock is released
// never reads the pending sign-in a poll is still writing.
func TestAccountViewDoesNotShareThePendingLogin(t *testing.T) {
	t.Setenv("FABO_SHARE_SERVER", "")
	p, err := OpenPublicShares(PublicShareOptions{ConfigPath: t.TempDir() + "/share.json"})
	if err != nil {
		t.Fatal(err)
	}
	p.login = &pendingLogin{Code: "ABCD"}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := range 2000 {
			p.mu.Lock()
			p.login.Error = fmt.Sprint("attempt ", i)
			p.mu.Unlock()
		}
	}()
	for range 2000 {
		if _, err := json.Marshal(p.report()); err != nil {
			t.Fatal(err)
		}
	}
	<-done
}
