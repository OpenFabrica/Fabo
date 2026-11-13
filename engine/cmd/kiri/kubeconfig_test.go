//go:build linux || darwin

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testK3s = `apiVersion: v1
clusters:
- cluster:
    certificate-authority-data: Q0E=
    server: https://127.0.0.1:6443
  name: default
contexts:
- context:
    cluster: default
    user: default
  name: default
current-context: default
kind: Config
users:
- name: default
  user:
    client-certificate-data: Q0VSVA==
    client-key-data: S0VZ
`

func TestKubeconfigPreservesOtherEntries(t *testing.T) {
	ours, e := kubeFromK3s(testK3s, "kiri-dev", 6450)
	if e != nil {
		t.Fatal(e)
	}
	existing := strings.ReplaceAll(testK3s, "default", "production") + "# keep this comment\n"
	for _, text := range []string{existing, strings.ReplaceAll(existing, "\n", "\r\n")} {
		merged, e := mergeKubeconfig(text, ours, "kiri-dev", false, false)
		if e != nil {
			t.Fatal(e)
		}
		if !strings.Contains(merged, "current-context: production") || !strings.Contains(merged, "https://127.0.0.1:6450") {
			t.Fatal(merged)
		}
		again, e := mergeKubeconfig(merged, ours, "kiri-dev", false, false)
		if e != nil || again != merged {
			t.Fatalf("not idempotent: %v\n%s", e, again)
		}
		removed, e := mergeKubeconfig(merged, "", "kiri-dev", false, true)
		if e != nil || removed != text {
			t.Fatalf("changed other entries: %v\n%s", e, removed)
		}
	}
	indented := "apiVersion: v1\nclusters:\n  - cluster:\n      server: https://other\n    name: 'other'\ncontexts: []\nusers: null\ncurrent-context: other\n"
	merged, e := mergeKubeconfig(indented, ours, "kiri-dev", true, false)
	if e != nil || !strings.Contains(merged, "\n  - cluster:\n      certificate-authority-data:") || !strings.Contains(merged, "current-context: kiri-dev") {
		t.Fatalf("%v\n%s", e, merged)
	}
	for _, text := range []string{`{"clusters":[]}`, "clusters: [{name: x}]\n", "clusters:\n  invalid: mapping\n", "clusters: []\nclusters: []\n"} {
		if _, e := mergeKubeconfig(text, ours, "kiri-dev", false, false); e == nil {
			t.Fatalf("accepted %s", text)
		}
	}
	removed, e := mergeKubeconfig(ours, "", "kiri-dev", false, true)
	if e != nil || !strings.Contains(removed, "current-context: \"\"") || !strings.Contains(removed, "clusters: null") {
		t.Fatalf("%v %s", e, removed)
	}
}
func TestKubeconfigPrivateSymlinkAndBackup(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "real")
	link := filepath.Join(root, "config")
	if e := os.WriteFile(target, []byte(testK3s), 0644); e != nil {
		t.Fatal(e)
	}
	if e := os.Symlink(target, link); e != nil {
		t.Fatal(e)
	}
	ours, _ := kubeFromK3s(testK3s, "kiri", 6448)
	changed, e := updateKubeconfig(link, ours, "kiri", false, false)
	if e != nil || !changed {
		t.Fatal(changed, e)
	}
	backup, e := os.ReadFile(target + ".bak.kiri")
	if e != nil || string(backup) != testK3s {
		t.Fatal(e)
	}
	info, e := os.Stat(target)
	if e != nil || info.Mode().Perm() != 0600 {
		t.Fatal(info, e)
	}
	info, _ = os.Lstat(link)
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("replaced symlink")
	}
	changed, e = updateKubeconfig(link, ours, "kiri", false, false)
	if e != nil || changed {
		t.Fatal(changed, e)
	}
	t.Setenv("KUBECONFIG", ":"+link+":/unused")
	if got := (installation{home: root}).kubePath(); got != link {
		t.Fatal(got)
	}
}
