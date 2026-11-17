package winhost

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/OpenFabrica/Fabo/engine/internal/redact"
)

// Settings → Diagnostics on Windows: a report of fabo-win's log
// (%LOCALAPPDATA%\Fabo\fabo-win.log) and the engine's in WSL, saved to the
// Desktop with secrets and the user's name taken out, and the logs folder
// in Explorer. Upstream Kiri's crates/kiri-win/src/diag.rs, without the
// page's log viewer and Kiri Cloud upload.

// DiagTailBytes is how much of each log a report keeps, from its end.
const DiagTailBytes = 512 << 10

// EngineLogScript prints the end of the engine's log in WSL: fabo-start
// (Fabo Linux) writes /var/log/fabo.log; under systemd (Ubuntu) the log
// is the journal's.
var EngineLogScript = fmt.Sprintf(`if [ -s /var/log/fabo.log ]; then tail -c %[1]d /var/log/fabo.log; `+
	`elif command -v journalctl >/dev/null 2>&1; then journalctl -u fabo --no-pager -o short-iso -n 5000 2>/dev/null | tail -c %[1]d; `+
	`else echo 'no /var/log/fabo.log and no journal' >&2; exit 1; fi`, DiagTailBytes)

// WSLShareConfig is the engine's share account file in WSL, whose token
// the report takes out wherever it appears.
const WSLShareConfig = "/var/lib/fabo-daemon/.fabo/share.json"

// DropCutLine drops the first line of a log's tail when the tail was cut
// (it is likely half a line).
func DropCutLine(text string, cut bool) string {
	if !cut {
		return text
	}
	if _, rest, ok := strings.Cut(text, "\n"); ok {
		return rest
	}
	return ""
}

// TailFile is the last n bytes of path, without a cut first line.
func TailFile(path string, n int64) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return "", err
	}
	cut := st.Size() > n
	if cut {
		if _, err := f.Seek(st.Size()-n, io.SeekStart); err != nil {
			return "", err
		}
	}
	b, err := io.ReadAll(io.LimitReader(f, n))
	if err != nil {
		return "", err
	}
	return DropCutLine(strings.ToValidUTF8(string(b), "�"), cut), nil
}

// ShareToken is the token in the engine's share.json, or "".
func ShareToken(b []byte) string {
	var cfg struct {
		Token string `json:"token"`
	}
	_ = json.Unmarshal(b, &cfg)
	return strings.TrimSpace(cfg.Token)
}

// ContainerLines is /containers/json?all=1 as `name  image  state` lines.
func ContainerLines(body []byte) ([]string, error) {
	var list []struct {
		Names []string
		Image string
		State string
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, err
	}
	lines := make([]string, 0, len(list))
	for _, c := range list {
		name := ""
		if len(c.Names) > 0 {
			name = strings.TrimPrefix(c.Names[0], "/")
		}
		lines = append(lines, fmt.Sprintf("%s  %s  %s", name, c.Image, c.State))
	}
	return lines, nil
}

// DiagReport is what a report holds. A part that could not be read has a
// note saying why instead of its text.
type DiagReport struct {
	Version, OS, Arch string
	// WithContainers: the person ticked "include the container list".
	WithContainers bool
	Containers     []string
	ContainersNote string
	WindowLog      string
	WindowNote     string
	EngineLog      string
	EngineNote     string
}

// Text is the report as saved. Everything goes through r last, as one
// text, so nothing reaches the file without being redacted.
func (d DiagReport) Text(r *redact.Redactor) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Fabo for Windows %s\n%s\n%s\n", d.Version, d.OS, d.Arch)
	if d.WithContainers {
		b.WriteString("\n== Containers ==\n")
		for _, c := range d.Containers {
			b.WriteString(c + "\n")
		}
		if d.ContainersNote != "" {
			fmt.Fprintf(&b, "(not read: %s)\n", d.ContainersNote)
		}
	}
	section := func(title, text, note string) {
		fmt.Fprintf(&b, "\n== %s log ==\n", title)
		if note != "" {
			fmt.Fprintf(&b, "(not read: %s)\n", note)
			return
		}
		b.WriteString(strings.TrimRight(text, "\n"))
		b.WriteString("\n")
	}
	section("Window", d.WindowLog, d.WindowNote)
	section("Engine", d.EngineLog, d.EngineNote)
	return r.Apply(b.String())
}

// ReportName is the report's file name for a moment: `Fabo report
// 2026-11-17 103000.txt`, in UTC as the logs are.
func ReportName(t time.Time) string {
	return "Fabo report " + t.UTC().Format("2006-01-02 150405") + ".txt"
}
