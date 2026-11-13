//go:build linux || darwin

package localengine

import (
	"encoding/csv"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
)

func TestClassicBuildOptionsPreserveMultipleTagsAndNestedDockerfile(t *testing.T) {
	o, err := parseBuildOptions(url.Values{
		"version": {"1"}, "t": {"fixture:one", "fixture:two"},
		"dockerfile": {"./nested/Dockerfile"}, "buildargs": {`{"EMPTY":"","UNSET":null}`},
	})
	if err != nil {
		t.Fatal(err)
	}
	args := o.command("/tmp/job")
	if !slices.Contains(args, "dockerfile=/tmp/job/context/nested") ||
		!slices.Contains(args, "filename=Dockerfile") ||
		!slices.Contains(args, "build-arg:EMPTY=") ||
		slices.Contains(args, "build-arg:UNSET=") {
		t.Fatalf("unexpected BuildKit arguments: %v", args)
	}
	output, err := csv.NewReader(strings.NewReader(args[len(args)-1])).Read()
	if err != nil || !slices.Contains(output, "name=fixture:one,fixture:two") {
		t.Fatalf("image exporter lost a tag: %v, %v", output, err)
	}
}

func TestBuildCancelTargetsOnlyTheRequestedBuild(t *testing.T) {
	e := &Engine{ctx: t.Context()}
	cancelled, finish, err := e.beginBuild(t.Context(), "cancel")
	if err != nil {
		t.Fatal(err)
	}
	defer finish()
	other, otherFinish, err := e.beginBuild(t.Context(), "other")
	if err != nil {
		t.Fatal(err)
	}
	defer otherFinish()

	if _, _, err := e.beginBuild(t.Context(), "cancel"); !errors.Is(err, errConflict) {
		t.Fatalf("duplicate active build ID returned %v", err)
	}
	w := httptest.NewRecorder()
	e.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/build/cancel?id=cancel", nil))
	if w.Code != http.StatusOK || cancelled.Err() == nil || other.Err() != nil {
		t.Fatalf("unexpected cancellation: HTTP %d, %v, %v", w.Code, cancelled.Err(), other.Err())
	}
}
