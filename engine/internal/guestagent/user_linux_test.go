package guestagent

import (
	"slices"
	"testing"
)

func TestLookupIdentity(t *testing.T) {
	passwd := "root:x:0:0:root:/root:/bin/sh\napp:x:1201:1202:App:/home/app:/bin/sh\n"
	groups := "tools:x:1203:app\nother:x:1204:someone\n"
	for _, tt := range []struct {
		raw      string
		uid, gid int
		groups   []int
	}{
		{"", 0, 0, nil}, {"app", 1201, 1202, []int{1203}},
		{"1201", 1201, 1202, []int{1203}}, {"app:tools", 1201, 1203, nil},
		{"2201", 2201, 2201, nil}, {"2201:2202", 2201, 2202, nil},
	} {
		got, err := lookupIdentity(tt.raw, passwd, groups)
		if err != nil || got.uid != tt.uid || got.gid != tt.gid || !slices.Equal(got.groups, tt.groups) {
			t.Errorf("lookup %q: %+v, %v", tt.raw, got, err)
		}
	}
	for _, bad := range []string{"missing", "app:missing", "app:", ":tools", "4294967295", "app:4294967295"} {
		if _, err := lookupIdentity(bad, passwd, groups); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestSupplementaryGroupsWithExplicitPrimaryGroup(t *testing.T) {
	const groups = "tools:x:1203:app\nother:x:1204:\n"
	app, err := lookupIdentity("app:1202", "app:x:1201:1202:App:/home/app:/bin/sh\n", groups)
	if err != nil {
		t.Fatal(err)
	}
	app, err = addSupplementaryGroups(app, []string{"other", "1205", "1204"}, groups)
	if err != nil || app.gid != 1202 || !slices.Equal(app.groups, []int{1204, 1205}) {
		t.Fatalf("wrong explicit groups: %+v, %v", app, err)
	}
	if _, err := addSupplementaryGroups(app, []string{"missing"}, groups); err == nil {
		t.Fatal("accepted unknown supplementary group")
	}
}
