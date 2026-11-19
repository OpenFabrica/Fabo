package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/egoist/mygo/ui"
)

// composeScratch is a folder for the path tests, by its real name (the
// macOS temporary folder is reached through a link).
func composeScratch(t *testing.T) string {
	t.Helper()
	d, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// TestComposeReadsAndWritesOnlyRealComposeFilesTheLabelsName: the labels
// are any container's to set, so only an absolute, local .yaml or .yml
// regular file is ever read or written.
func TestComposeReadsAndWritesOnlyRealComposeFilesTheLabelsName(t *testing.T) {
	d := composeScratch(t)
	f := filepath.Join(d, "compose.yaml")
	for name, text := range map[string]string{"compose.yaml": "services: {}\n", "secret.txt": "x"} {
		if err := os.WriteFile(filepath.Join(d, name), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(d, "dir.yaml"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got, ok := composeFile(f); !ok || got != f {
		t.Errorf("a compose file: %q %v", got, ok)
	}
	for why, bad := range map[string]string{
		"relative": "compose.yaml",
		"not yaml": filepath.Join(d, "secret.txt"),
		"a folder": filepath.Join(d, "dir.yaml"),
		"missing":  filepath.Join(d, "gone.yaml"),
		"empty":    "",
	} {
		if got, ok := composeFile(bad); ok {
			t.Errorf("%s: %s accepted as %q", why, bad, got)
		}
	}
	if runtime.GOOS != "windows" {
		if err := os.Symlink(filepath.Join(d, "secret.txt"), filepath.Join(d, "evil.yaml")); err != nil {
			t.Fatal(err)
		}
		if got, ok := composeFile(filepath.Join(d, "evil.yaml")); ok {
			t.Errorf("a .yaml link to another kind of file: %q", got)
		}
		if err := os.Symlink(f, filepath.Join(d, "link.yml")); err != nil {
			t.Fatal(err)
		}
		if got, ok := composeFile(filepath.Join(d, "link.yml")); !ok || got != f {
			t.Errorf("a link to a compose file is its target: %q %v", got, ok)
		}
		if err := os.Symlink("/dev/zero", filepath.Join(d, "device.yaml")); err != nil {
			t.Fatal(err)
		}
		if got, ok := composeFile(filepath.Join(d, "device.yaml")); ok {
			t.Errorf("a device: %q", got)
		}
	}
}

// TestComposeLabelNeverNamesANetworkPath: a network path, however it is
// written, is refused by its words alone, before anything looks at it.
func TestComposeLabelNeverNamesANetworkPath(t *testing.T) {
	for _, f := range []string{
		`\\host\share\compose.yaml`, "//host/share/compose.yaml", `\\?\UNC\host\share\compose.yaml`, `\\.\pipe\x.yaml`,
		"/net/h/x.yaml", "/usr/../net/host/x.yaml", "/./net/h", "/a/b/../../net/h", "//net/h/x", "/NET/h/x.yaml", "/net", "/x/..//net/./h",
	} {
		if !remotePath(f) {
			t.Errorf("%s is a network path", f)
		}
		if _, ok := composeFile(f); ok {
			t.Errorf("%s read as a compose file", f)
		}
		if _, ok := composeDir(f); ok {
			t.Errorf("%s taken as a folder", f)
		}
	}
	for _, f := range []string{`C:\w\compose.yaml`, `\\?\C:\w\compose.yaml`, "/w/compose.yaml", "/network/x.yaml", "/srv/net/x.yaml", "/usr/net", "net/x"} {
		if remotePath(f) {
			t.Errorf("%s is local", f)
		}
	}
}

// TestComposeProjectFolderFallsBackToTheFilesOwn: a folder label that is
// not a local folder is never used; the first file's folder is, as
// `docker compose -f` alone would take; with no good file, nothing is.
func TestComposeProjectFolderFallsBackToTheFilesOwn(t *testing.T) {
	d := composeScratch(t)
	f := filepath.Join(d, "compose.yaml")
	if err := os.WriteFile(f, []byte("services: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	other := composeScratch(t)
	if got, ok := projectFolder(composeProject{workDir: other, files: []string{f}}); !ok || got != other {
		t.Errorf("a local folder label: %q %v", got, ok)
	}
	for _, dir := range []string{"", "relative", "/net/host/share", "//host/share", f, filepath.Join(d, "gone")} {
		if got, ok := projectFolder(composeProject{workDir: dir, files: []string{f}}); !ok || got != d {
			t.Errorf("folder label %q: %q %v", dir, got, ok)
		}
	}
	if got, ok := projectFolder(composeProject{workDir: "/net/host/share", files: []string{"/net/host/share/compose.yaml"}}); ok {
		t.Errorf("no local folder or file, yet %q", got)
	}
	// Nothing runs for a project whose labels name another kind of file.
	secret := filepath.Join(d, "secret.txt")
	if err := os.WriteFile(secret, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := composeUp(context.Background(), composeProject{name: "x", workDir: d, files: []string{f, secret}}); !errors.Is(err, errComposePath) {
		t.Errorf("compose up with %s: %v", secret, err)
	}
}

// TestComposeConfigTabRefusesAFileTheLabelsShouldNotName: the Config tab
// says why, shows no editor, and a file swapped for a link to another
// kind of file between reading and Save is left as it is.
func TestComposeConfigTabRefusesAFileTheLabelsShouldNotName(t *testing.T) {
	_, s := newFakeEngine(t)
	d := composeScratch(t)
	secret := filepath.Join(d, "secret.txt")
	if err := os.WriteFile(secret, []byte("token=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	label := func(path string) {
		for i := range s.containers {
			if s.containers[i].Project == "shop" {
				s.containers[i].ConfigFiles = path
			}
		}
	}
	label(secret)
	s.selectRow(projectKey("shop"))
	s.tab[containersPage+"/project"] = 1
	tt := ui.NewTester(s.view, 1180, 760)
	tt.Frame()
	if s.compose.loaded || !tt.HasText(errComposePath.Error()) || tt.HasText(secret+"\n") {
		t.Fatalf("loaded %v, problem %q", s.compose.loaded, s.compose.problem)
	}
	if _, found := tt.Find("Save"); found {
		t.Fatal("Save offered for a file Fabo won't write")
	}
	if runtime.GOOS == "windows" {
		return
	}
	f := filepath.Join(d, "compose.yaml")
	if err := os.WriteFile(f, []byte("services: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	label(f)
	tt.Frame()
	if !s.compose.loaded {
		t.Fatalf("a real compose file was not read: %q", s.compose.problem)
	}
	if err := os.Remove(f); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, f); err != nil {
		t.Fatal(err)
	}
	s.compose.text = "services:\n  web: {}\n"
	tt.Frame()
	if err := tt.Click("Save"); err != nil {
		t.Fatal(err)
	}
	// The failure is a toast: the refresh after Save reads the labels again.
	tt.Frame()
	if data, _ := os.ReadFile(secret); string(data) != "token=1\n" || !tt.HasText(errComposePath.Error()) {
		t.Fatalf("secret %q, texts %q", data, tt.Texts())
	}
}
