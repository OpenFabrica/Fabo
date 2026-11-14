package guestagent

import "testing"

// An exec follows BuildKit into its leaf cgroup without ever joining a
// sibling or ancestor cgroup.
func TestLiveExecCgroupStaysInsideContainer(t *testing.T) {
	root := "/sys/fs/cgroup/fabo/fabo-builder"
	for _, tt := range []struct{ membership, want string }{
		{"0::/fabo/fabo-builder\n", root},
		{"0::/fabo/fabo-builder/init\n", root + "/init"},
		{"0::/fabo/fabo-builder-other/init\n", ""},
		{"0::/fabo\n", ""},
		{"0::fabo/fabo-builder/init\n", ""},
		{"1:memory:/fabo/fabo-builder\n", ""},
	} {
		got, err := liveExecCgroup(root, tt.membership)
		if got != tt.want || (err != nil) != (tt.want == "") {
			t.Errorf("membership %q: got %q, %v, want %q", tt.membership, got, err, tt.want)
		}
	}
}
