//go:build linux || darwin

package localengine

import (
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
)

func TestPushReceiptFollowsImageIdentity(t *testing.T) {
	target, err := name.NewTag("registry.example/team/app:v1")
	if err != nil {
		t.Fatal(err)
	}
	first := Image{ID: "sha256:" + strings.Repeat("1", 64)}
	replacement := Image{ID: "sha256:" + strings.Repeat("2", 64)}
	digest := "sha256:" + strings.Repeat("d", 64)
	e := &Engine{}
	put := func(image Image) {
		t.Helper()
		if err := e.putImage(target.Name(), image); err != nil {
			t.Fatal(err)
		}
	}
	put(first)
	if err := e.recordPushedDigest(target, first.ID, digest); err != nil {
		t.Fatal(err)
	}
	put(first)
	if got := e.images[target.Name()].RepoDigest; got != target.Context().Name()+"@"+digest {
		t.Fatalf("re-tagging the same image lost its repository digest: %q", got)
	}
	put(replacement)
	if err := e.recordPushedDigest(target, first.ID, digest); err != nil {
		t.Fatal(err)
	}
	if got := e.images[target.Name()]; got.ID != replacement.ID || got.RepoDigest != "" {
		t.Fatalf("the previous push changed the replacement tag: %+v", got)
	}
}
