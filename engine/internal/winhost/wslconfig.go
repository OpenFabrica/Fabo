package winhost

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"
)

// Settings → Resources on Windows. Fabo's engine runs in WSL's one VM,
// whose CPUs and memory are `processors` and `memory` under [wsl2] in
// %USERPROFILE%\.wslconfig (memory is half of the PC's when unset). Saving
// changes those two values and keeps every other byte of the file; they
// apply when WSL restarts, which stops every WSL distro, not only Fabo's.
// Upstream Kiri's crates/kiri-win/src/wslconfig.rs, made to keep the
// file's encoding, line endings and comments, and to write atomically.

// WSLResources is the control pipe's GET /resources, shaped as the
// engine's /fabo/resources so the window's Resources pane reads both.
type WSLResources struct {
	CPUs          int  `json:"cpus"`
	Memory        int  `json:"memory_mib"`
	RunningCPUs   int  `json:"running_cpus"`
	RunningMemory int  `json:"running_memory_mib"`
	Awake         bool `json:"awake"`
	MaxCPUs       int  `json:"max_cpus"`
	MaxMemory     int  `json:"max_memory_mib"`
}

// ErrWSLConfigEncoding is a .wslconfig that is not UTF-8: rewriting it as
// UTF-8 could change what WSL reads, so Fabo leaves it alone.
var ErrWSLConfigEncoding = errors.New(".wslconfig is not saved as UTF-8; Fabo will not rewrite it. Save it as UTF-8 or set processors and memory under [wsl2] by hand")

// utf8BOM starts a file Notepad saved as "UTF-8 with BOM".
var utf8BOM = []byte{0xef, 0xbb, 0xbf}

// wslConfigMu serialises read-modify-write of .wslconfig within fabo-win.
var wslConfigMu sync.Mutex

// decodeWSLConfig splits a .wslconfig's bytes into its BOM and its text.
// UTF-16 and other non-UTF-8 files are refused.
func decodeWSLConfig(b []byte) (bom bool, text string, err error) {
	if len(b) >= 2 && (b[0] == 0xff && b[1] == 0xfe || b[0] == 0xfe && b[1] == 0xff) {
		return false, "", ErrWSLConfigEncoding
	}
	if bytes.HasPrefix(b, utf8BOM) {
		bom, b = true, b[len(utf8BOM):]
	}
	// UTF-16 without a BOM is valid UTF-8 byte for byte, but has NULs.
	if !utf8.Valid(b) || bytes.IndexByte(b, 0) >= 0 {
		return false, "", ErrWSLConfigEncoding
	}
	return bom, string(b), nil
}

// iniLine is one line of the file: its text and its own line ending, kept
// so lines Fabo does not change are written back byte for byte.
type iniLine struct{ text, eol string }

// splitLines splits text keeping each line's ending ("\r\n", "\n" or ""
// for a last line without one).
func splitLines(text string) []iniLine {
	var lines []iniLine
	for text != "" {
		i := strings.IndexByte(text, '\n')
		if i < 0 {
			lines = append(lines, iniLine{text, ""})
			break
		}
		line := text[:i]
		eol := "\n"
		if strings.HasSuffix(line, "\r") {
			line, eol = line[:len(line)-1], "\r\n"
		}
		lines = append(lines, iniLine{line, eol})
		text = text[i+1:]
	}
	return lines
}

// lineEnding is the ending new lines get: the file's own, CRLF for a new
// file (Windows' own).
func lineEnding(text string) string {
	if !strings.Contains(text, "\r\n") && strings.Contains(text, "\n") {
		return "\n"
	}
	return "\r\n"
}

// section is the name of a `[name]` header line, and whether it is one.
// A trailing `# comment` after the bracket is allowed.
func section(line string) (string, bool) {
	l := strings.TrimSpace(line)
	if !strings.HasPrefix(l, "[") {
		return "", false
	}
	end := strings.IndexByte(l, ']')
	if end < 0 {
		return strings.TrimSpace(l[1:]), true
	}
	return strings.TrimSpace(l[1:end]), true
}

// keyValue splits a `key = value # comment` line: the key, and the byte
// span of the value within the line. Comment lines are not key lines.
func keyValue(line string) (key string, start, end int, ok bool) {
	l := strings.TrimSpace(line)
	if l == "" || l[0] == '#' || l[0] == ';' {
		return "", 0, 0, false
	}
	eq := strings.IndexByte(line, '=')
	if eq < 0 {
		return "", 0, 0, false
	}
	after := line[eq+1:]
	raw := after
	if c := strings.IndexByte(after, '#'); c >= 0 {
		raw = after[:c]
	}
	lead := len(raw) - len(strings.TrimLeft(raw, " \t"))
	value := strings.TrimSpace(raw)
	start = eq + 1 + lead
	return strings.TrimSpace(line[:eq]), start, start + len(value), true
}

// WSLConfigValue is key's value under [wsl2] (any case), without an inline
// comment or surrounding quotes. With the key set twice, the last wins.
func WSLConfigValue(text, key string) (string, bool) {
	found, value, inWSL2 := false, "", false
	for _, l := range splitLines(text) {
		if name, ok := section(l.text); ok {
			inWSL2 = strings.EqualFold(name, "wsl2")
			continue
		}
		if k, s, e, ok := keyValue(l.text); inWSL2 && ok && strings.EqualFold(k, key) {
			found, value = true, strings.Trim(l.text[s:e], `"`)
		}
	}
	return value, found
}

// SetWSLConfigValue is text with key's value under [wsl2] set to value.
// Every place the key is set there is changed in place, keeping its
// indentation, spelling and comment; a missing key goes after the
// section's last line, and a missing section at the end. Lines not
// changed keep their bytes and endings.
func SetWSLConfigValue(text, key, value string) string {
	lines := splitLines(text)
	eol := lineEnding(text)
	inWSL2, done := false, false
	lastInFirst := -1 // the first [wsl2]'s last non-blank line
	firstOpen := false
	for i, l := range lines {
		if name, ok := section(l.text); ok {
			inWSL2 = strings.EqualFold(name, "wsl2")
			if inWSL2 && lastInFirst < 0 {
				lastInFirst, firstOpen = i, true
			} else {
				firstOpen = false
			}
			continue
		}
		if !inWSL2 {
			continue
		}
		if firstOpen && strings.TrimSpace(l.text) != "" {
			lastInFirst = i
		}
		if k, s, e, ok := keyValue(l.text); ok && strings.EqualFold(k, key) {
			lines[i].text = l.text[:s] + value + l.text[e:]
			done = true
		}
	}
	if !done {
		add := []iniLine{{key + "=" + value, eol}}
		at := len(lines)
		if lastInFirst >= 0 {
			at = lastInFirst + 1
		} else {
			// No [wsl2] yet: a new section at the end, after a blank line.
			add = append([]iniLine{{"[wsl2]", eol}}, add...)
			if n := len(lines); n > 0 && strings.TrimSpace(lines[n-1].text) != "" {
				add = append([]iniLine{{"", eol}}, add...)
			}
		}
		// The line before the new ones needs an ending of its own.
		if at > 0 && lines[at-1].eol == "" {
			lines[at-1].eol = eol
		}
		lines = append(lines[:at], append(add, lines[at:]...)...)
	}
	var b strings.Builder
	for _, l := range lines {
		b.WriteString(l.text)
		b.WriteString(l.eol)
	}
	return b.String()
}

// ParseMiB reads a .wslconfig size (32GB, 8192MB, 512m, bytes) as MiB.
func ParseMiB(v string) (int, bool) {
	v = strings.ToUpper(strings.TrimSpace(v))
	digits := 0
	for digits < len(v) && v[digits] >= '0' && v[digits] <= '9' {
		digits++
	}
	n, err := strconv.ParseInt(v[:digits], 10, 64)
	if err != nil {
		return 0, false
	}
	switch strings.TrimSuffix(strings.TrimSpace(v[digits:]), "B") {
	case "T":
		return int(n << 20), true
	case "G":
		return int(n << 10), true
	case "M":
		return int(n), true
	case "K":
		return int(n >> 10), true
	case "":
		return int(n >> 20), true
	}
	return 0, false
}

// MemoryValue writes MiB as .wslconfig does: whole GB when it can.
func MemoryValue(mib int) string {
	if mib%1024 == 0 {
		return fmt.Sprintf("%dGB", mib/1024)
	}
	return fmt.Sprintf("%dMB", mib)
}

// AsSet is the memory the VM runs with as Settings shows it. Linux reports
// less than WSL gave it (the kernel keeps some back, 1.2 GB of 64 GB), so
// up to a sixteenth under the setting is the setting; anything else is
// rounded up to a whole GB.
func AsSet(runningMiB, setMiB int) int {
	if runningMiB <= setMiB && runningMiB+setMiB/16 >= setMiB {
		return setMiB
	}
	return (runningMiB + 1023) / 1024 * 1024
}

// ParseRunning reads `nproc; head -1 /proc/meminfo` from the VM: its CPUs
// and its memory in MiB.
func ParseRunning(out string) (cpus, mib int, ok bool) {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) < 2 {
		return 0, 0, false
	}
	cpus, err := strconv.Atoi(strings.TrimSpace(lines[0]))
	if err != nil {
		return 0, 0, false
	}
	f := strings.Fields(lines[1])
	if len(f) < 2 || f[0] != "MemTotal:" {
		return 0, 0, false
	}
	kib, err := strconv.Atoi(f[1])
	if err != nil {
		return 0, 0, false
	}
	return cpus, kib >> 10, true
}

// ReadWSLResources is what Settings shows: the values in text (a missing
// key is WSL's default: every CPU, half the memory), the PC's limits, and
// what the VM runs with now, when it runs (running nil: it does not).
func ReadWSLResources(text string, maxCPUs, maxMiB int, running *[2]int) WSLResources {
	maxCPUs = max(1, maxCPUs)
	r := WSLResources{CPUs: maxCPUs, Memory: maxMiB / 2, MaxCPUs: maxCPUs, MaxMemory: maxMiB}
	if v, ok := WSLConfigValue(text, "processors"); ok {
		if n, err := strconv.Atoi(v); err == nil {
			r.CPUs = min(max(n, 1), maxCPUs)
		}
	}
	if v, ok := WSLConfigValue(text, "memory"); ok {
		if m, ok := ParseMiB(v); ok && m > 0 {
			r.Memory = m
		}
	}
	r.RunningCPUs, r.RunningMemory = r.CPUs, r.Memory
	if running != nil {
		r.Awake = true
		r.RunningCPUs, r.RunningMemory = running[0], AsSet(running[1], r.Memory)
	}
	return r
}

// CheckWSLResources refuses values WSL cannot use.
func CheckWSLResources(cpus, mib, maxCPUs, maxMiB int) error {
	if cpus < 1 || cpus > max(1, maxCPUs) {
		return fmt.Errorf("CPUs must be between 1 and %d", max(1, maxCPUs))
	}
	if mib < 1024 || maxMiB > 0 && mib > maxMiB {
		return fmt.Errorf("memory must be between 1 GB and %d MB", maxMiB)
	}
	return nil
}

// ReadWSLConfigText reads path's text for display; a missing file is
// empty. A file Fabo cannot read or decode is an error.
func ReadWSLConfigText(path string) (string, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	_, text, err := decodeWSLConfig(b)
	return text, err
}

// SaveWSLResources writes processors and memory under [wsl2] in path,
// changing nothing else, and reports whether the file changed. Only a
// missing file counts as empty: any other read failure stops the save,
// so the user's file is never replaced by two keys. The new file is
// written beside the old and renamed over it, so a crash leaves one or
// the other whole.
func SaveWSLResources(path string, cpus, mib int) (bool, error) {
	wslConfigMu.Lock()
	defer wslConfigMu.Unlock()
	// Write through a link to the file it names, not over the link.
	if real, err := filepath.EvalSymlinks(path); err == nil {
		path = real
	}
	b, err := os.ReadFile(path)
	mode := fs.FileMode(0o644)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		b = nil
	case err != nil:
		return false, fmt.Errorf("could not read .wslconfig: %w", err)
	default:
		if st, err := os.Stat(path); err == nil {
			mode = st.Mode().Perm()
		}
	}
	bom, text, err := decodeWSLConfig(b)
	if err != nil {
		return false, err
	}
	next := text
	if v, ok := WSLConfigValue(next, "processors"); !ok || strings.TrimSpace(v) != strconv.Itoa(cpus) {
		next = SetWSLConfigValue(next, "processors", strconv.Itoa(cpus))
	}
	if v, ok := WSLConfigValue(next, "memory"); !ok || !sameMiB(v, mib) {
		next = SetWSLConfigValue(next, "memory", MemoryValue(mib))
	}
	if next == text && b != nil {
		return false, nil
	}
	out := []byte(next)
	if bom {
		out = append(append([]byte{}, utf8BOM...), out...)
	}
	return true, writeAtomic(path, out, mode)
}

// sameMiB reports whether the size v already means mib (8192MB is 8GB).
func sameMiB(v string, mib int) bool {
	m, ok := ParseMiB(v)
	return ok && m == mib
}

// writeAtomic replaces path with data through a temporary file in the same
// folder (os.Rename replaces an existing file on Windows too).
func writeAtomic(path string, data []byte, mode fs.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".wslconfig-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	_, err = tmp.Write(data)
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(name, mode)
	}
	if err == nil {
		err = os.Rename(name, path)
	}
	if err != nil {
		os.Remove(name)
	}
	return err
}
