package winhost

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// TestWSLConfigKeepsTheRest sets keys under [wsl2] and leaves every other
// byte as it was (upstream's case, plus endings, comments and casing).
func TestWSLConfigKeepsTheRest(t *testing.T) {
	text := "[wsl2]\r\nmemory=32GB\r\n\r\n[experimental]\r\nsparseVhd=true\r\n"
	if v, _ := WSLConfigValue(text, "memory"); v != "32GB" {
		t.Fatalf("memory = %q", v)
	}
	got := SetWSLConfigValue(SetWSLConfigValue(text, "memory", "16GB"), "processors", "8")
	if want := "[wsl2]\r\nmemory=16GB\r\nprocessors=8\r\n\r\n[experimental]\r\nsparseVhd=true\r\n"; got != want {
		t.Fatalf("got %q\nwant %q", got, want)
	}
	for _, c := range []struct{ in, key, value, want string }{
		// A new file is Windows' CRLF.
		{"", "memory", "4GB", "[wsl2]\r\nmemory=4GB\r\n"},
		{"[boot]\r\nx=1\r\n", "memory", "4GB", "[boot]\r\nx=1\r\n\r\n[wsl2]\r\nmemory=4GB\r\n"},
		// LF files stay LF; a missing last ending is added only where a
		// line follows.
		{"[wsl2]\nswap=0\n", "memory", "4GB", "[wsl2]\nswap=0\nmemory=4GB\n"},
		{"[wsl2]\nswap=0", "memory", "4GB", "[wsl2]\nswap=0\nmemory=4GB\n"},
		{"[wsl2]\nmemory=8GB", "memory", "4GB", "[wsl2]\nmemory=4GB"},
		// Indentation, key spelling, spaces and the inline comment stay.
		{"[WSL2]\n  Memory = 8GB   # half\n", "memory", "4GB", "[WSL2]\n  Memory = 4GB   # half\n"},
		// Commented-out keys are not the key.
		{"[wsl2]\n# memory=2GB\nswap=0\n", "memory", "4GB", "[wsl2]\n# memory=2GB\nswap=0\nmemory=4GB\n"},
		// After the section's last line, before the blank lines and the next section.
		{"# top\n[wsl2]\nswap=0\n\n\n[experimental]\na=1\n", "processors", "2", "# top\n[wsl2]\nswap=0\nprocessors=2\n\n\n[experimental]\na=1\n"},
		// A header with a comment is still [wsl2].
		{"[wsl2] # mine\nswap=0\n", "processors", "2", "[wsl2] # mine\nswap=0\nprocessors=2\n"},
		// The key only under [wsl2]: another section's is left.
		{"[experimental]\nmemory=1GB\n", "memory", "4GB", "[experimental]\nmemory=1GB\n\n[wsl2]\nmemory=4GB\n"},
		// Mixed endings: each kept; new lines use CRLF.
		{"[wsl2]\r\nswap=0\nkernel=x\r\n", "memory", "4GB", "[wsl2]\r\nswap=0\nkernel=x\r\nmemory=4GB\r\n"},
		// Set twice: both change, so no stale value is left to win.
		{"[wsl2]\nmemory=1GB\n[wsl2]\nmemory=2GB\n", "memory", "4GB", "[wsl2]\nmemory=4GB\n[wsl2]\nmemory=4GB\n"},
	} {
		if got := SetWSLConfigValue(c.in, c.key, c.value); got != c.want {
			t.Errorf("%q: got %q want %q", c.in, got, c.want)
		}
	}
	if v, _ := WSLConfigValue("[wsl2]\nmemory=1GB\nmemory=\"6GB\" # x\n", "memory"); v != "6GB" {
		t.Fatalf("last value = %q", v)
	}
	if _, ok := WSLConfigValue("[boot]\nmemory=1GB\n", "memory"); ok {
		t.Fatal("found outside [wsl2]")
	}
}

// TestWSLSizes reads and writes sizes as .wslconfig spells them.
func TestWSLSizes(t *testing.T) {
	for in, want := range map[string]int{"32GB": 32768, "8192MB": 8192, "4g": 4096, "1TB": 1 << 20, "2097152KB": 2048, "1073741824": 1024} {
		if got, ok := ParseMiB(in); !ok || got != want {
			t.Errorf("ParseMiB(%q) = %d %v", in, got, ok)
		}
	}
	if _, ok := ParseMiB("lots"); ok {
		t.Fatal("lots parsed")
	}
	if MemoryValue(16384) != "16GB" || MemoryValue(1536) != "1536MB" {
		t.Fatal(MemoryValue(16384), MemoryValue(1536))
	}
}

// TestAVMRunningAsSetShowsItsSetting is upstream's as_set test.
func TestAVMRunningAsSetShowsItsSetting(t *testing.T) {
	for _, c := range []struct{ running, set, want int }{
		{65843312 >> 10, 65536, 65536},
		{8000, 8192, 8192},
		{65843312 >> 10, 32768, 63 * 1024},
		{16000, 65536, 16 * 1024},
	} {
		if got := AsSet(c.running, c.set); got != c.want {
			t.Errorf("AsSet(%d, %d) = %d, want %d", c.running, c.set, got, c.want)
		}
	}
}

// TestReadWSLResources fills in WSL's defaults and the running VM.
func TestReadWSLResources(t *testing.T) {
	r := ReadWSLResources("", 8, 16384, nil)
	if r.CPUs != 8 || r.Memory != 8192 || r.Awake || r.RunningCPUs != 8 || r.MaxMemory != 16384 {
		t.Fatalf("%+v", r)
	}
	r = ReadWSLResources("[wsl2]\nprocessors=99\nmemory=4GB\n", 8, 16384, &[2]int{4, 4000})
	if r.CPUs != 8 || r.Memory != 4096 || !r.Awake || r.RunningCPUs != 4 || r.RunningMemory != 4096 {
		t.Fatalf("%+v", r)
	}
	if cpus, mib, ok := ParseRunning("12\nMemTotal:       16303512 kB\n"); !ok || cpus != 12 || mib != 15921 {
		t.Fatal(cpus, mib, ok)
	}
	if _, _, ok := ParseRunning("nope"); ok {
		t.Fatal("parsed nonsense")
	}
	if CheckWSLResources(0, 4096, 8, 16384) == nil || CheckWSLResources(2, 512, 8, 16384) == nil || CheckWSLResources(2, 32768, 8, 16384) == nil || CheckWSLResources(8, 16384, 8, 16384) != nil {
		t.Fatal("bounds")
	}
}

// TestSaveWSLResources writes only what changed, keeps the BOM and the
// rest, refuses UTF-16, and never replaces a file it could not read.
func TestSaveWSLResources(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".wslconfig")

	// No file yet: one is made.
	if changed, err := SaveWSLResources(path, 4, 8192); err != nil || !changed {
		t.Fatal(changed, err)
	}
	if b, _ := os.ReadFile(path); string(b) != "[wsl2]\r\nprocessors=4\r\nmemory=8GB\r\n" {
		t.Fatalf("%q", b)
	}

	// A BOM, LF endings, a comment and other sections stay; 8192MB is 8 GB already.
	orig := "\xef\xbb\xbf[wsl2]\nmemory=8192MB # keep\nprocessors=2\n\n[experimental]\nautoMemoryReclaim=gradual\n"
	os.WriteFile(path, []byte(orig), 0o644)
	if changed, err := SaveWSLResources(path, 2, 8192); err != nil || changed {
		t.Fatal("unchanged values rewrote the file", changed, err)
	}
	if changed, err := SaveWSLResources(path, 6, 8192); err != nil || !changed {
		t.Fatal(changed, err)
	}
	if b, _ := os.ReadFile(path); string(b) != "\xef\xbb\xbf[wsl2]\nmemory=8192MB # keep\nprocessors=6\n\n[experimental]\nautoMemoryReclaim=gradual\n" {
		t.Fatalf("%q", b)
	}

	// UTF-16 and other non-UTF-8 files are left alone.
	for _, b := range [][]byte{{0xff, 0xfe, '[', 0, 'w', 0}, {'[', 0, 'w', 0, 's', 0}, {'[', 0xff, ']'}} {
		os.WriteFile(path, b, 0o644)
		if _, err := SaveWSLResources(path, 4, 4096); !errors.Is(err, ErrWSLConfigEncoding) {
			t.Fatalf("%q: %v", b, err)
		}
		if got, _ := os.ReadFile(path); string(got) != string(b) {
			t.Fatalf("rewrote %q as %q", b, got)
		}
		if _, err := ReadWSLConfigText(path); err == nil {
			t.Fatal("read a non-UTF-8 file")
		}
	}

	// A file that cannot be read is not replaced.
	if runtime.GOOS != "windows" && os.Getuid() != 0 {
		os.WriteFile(path, []byte("[wsl2]\nswap=0\n"), 0o644)
		os.Chmod(path, 0)
		_, err := SaveWSLResources(path, 4, 4096)
		os.Chmod(path, 0o644)
		if err == nil {
			t.Fatal("saved over an unreadable file")
		}
		if got, _ := os.ReadFile(path); string(got) != "[wsl2]\nswap=0\n" {
			t.Fatalf("%q", got)
		}
	}

	// No temporary file is left behind.
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("%d files", len(entries))
	}
}
