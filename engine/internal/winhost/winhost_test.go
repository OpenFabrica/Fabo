package winhost

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"unicode/utf16"
)

// utf16le encodes s as wsl.exe prints it.
func utf16le(s string) []byte {
	var b []byte
	for _, u := range utf16.Encode([]rune(s)) {
		b = append(b, byte(u), byte(u>>8))
	}
	return b
}

// TestDistros reads `wsl -l -q` in both of WSL's encodings.
func TestDistros(t *testing.T) {
	want := []string{"fabo", "Ubuntu-26.04"}
	for _, out := range [][]byte{utf16le("\ufefffabo\r\nUbuntu-26.04\r\n\r\n"), []byte("fabo\nUbuntu-26.04\n")} {
		if got := Distros(out); !reflect.DeepEqual(got, want) {
			t.Fatalf("Distros(%q) = %q", out, got)
		}
	}
	if got := FirstLine(utf16le("\r\nThe distribution was not found.\r\n"), "x"); got != "The distribution was not found." {
		t.Fatalf("FirstLine = %q", got)
	}
}

// TestSavedRoundTrip keeps the fields fabo.ps1 reads.
func TestSavedRoundTrip(t *testing.T) {
	dir := t.TempDir()
	if s := LoadSaved(dir); s.Done || s.Distro != "" {
		t.Fatalf("fresh = %+v", s)
	}
	on := true
	if err := SaveSaved(dir, Saved{Done: true, Linux: "fabo", Distro: "fabo", Autostart: &on}); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "setup.json"))
	if !strings.Contains(string(raw), `"distro": "fabo"`) {
		t.Fatalf("setup.json = %s", raw)
	}
	if s := LoadSaved(dir); !s.Done || s.Distro != "fabo" || s.Autostart == nil || !*s.Autostart {
		t.Fatalf("loaded %+v", s)
	}
}

// TestManifest refuses a changed or missing file and a release build
// without checksums.
func TestManifest(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "bin"), 0o755)
	os.WriteFile(filepath.Join(dir, "bin", "docker.exe"), []byte("docker"), 0o644)
	sum, _ := FileSHA256(filepath.Join(dir, "bin", "docker.exe"))
	m := Manifest("install.sh=00;bin/docker.exe=" + sum)
	if p, err := m.Verified(dir, "bin/docker.exe", true); err != nil || !strings.HasSuffix(p, "docker.exe") {
		t.Fatalf("good file: %q, %v", p, err)
	}
	os.WriteFile(filepath.Join(dir, "bin", "docker.exe"), []byte("evil"), 0o644)
	if _, err := m.Verified(dir, "bin/docker.exe", true); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("changed file: %v", err)
	}
	if _, err := m.Verified(dir, "install.sh", true); err == nil || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("missing file: %v", err)
	}
	if _, err := m.Verified(dir, "fabo", true); err == nil {
		t.Fatal("file outside the manifest accepted")
	}
	if _, err := Manifest("").Verified(dir, "bin/docker.exe", true); err != ErrNoManifest {
		t.Fatalf("release without manifest: %v", err)
	}
	if _, err := Manifest("").Verified(dir, "bin/docker.exe", false); err != nil {
		t.Fatalf("development build: %v", err)
	}
}

// TestParseSystemProxy covers Windows' two ProxyServer forms.
func TestParseSystemProxy(t *testing.T) {
	p := ParseSystemProxy(true, "127.0.0.1:7890", "localhost;*.corp;<local>", false)
	if *p.HTTP != "127.0.0.1:7890" || *p.HTTPS != "127.0.0.1:7890" || !reflect.DeepEqual(p.NoProxy, []string{"localhost", "*.corp"}) {
		t.Fatalf("single = %+v", p)
	}
	p = ParseSystemProxy(true, "http=proxy:80;https=secure:443;ftp=x:21", "", true)
	if *p.HTTP != "proxy:80" || *p.HTTPS != "secure:443" || !p.PAC {
		t.Fatalf("per scheme = %+v", p)
	}
	p = ParseSystemProxy(false, "proxy:80", "a", false)
	if p.HTTP != nil || p.HTTPS != nil || len(p.NoProxy) != 0 {
		t.Fatalf("disabled = %+v", p)
	}
}

// TestWindowsPathInWSL maps drive letters as WSL's automount does.
func TestWindowsPathInWSL(t *testing.T) {
	if got := WindowsPathInWSL(`C:\Users\Me\AppData\Local\Fabo\install.sh`); got != "/mnt/c/Users/Me/AppData/Local/Fabo/install.sh" {
		t.Fatal(got)
	}
}
