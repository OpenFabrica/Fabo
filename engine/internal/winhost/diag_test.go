package winhost

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/OpenFabrica/Fabo/engine/internal/redact"
)

// TestDiagReportIsRedacted plants what must not leave the PC in every part
// of a report and checks none of it survives.
func TestDiagReportIsRedacted(t *testing.T) {
	token := "ft_live0123456789abcdefSECRET"
	odd := "plain-token-without-prefix-42"
	shareJSON := []byte(`{"token":"` + odd + `","login":"ada"}`)
	d := DiagReport{
		Version: "1.2.3", OS: "Microsoft Windows [Version 10.0.26100.1]", Arch: "amd64",
		WithContainers: true,
		Containers:     []string{"db  postgres:16  running"},
		WindowLog: `2026-11-17T10:00:00.000Z setup: copied C:\Users\Ada\Downloads\x into WSL
2026-11-17T10:00:01.000Z proxy http://bob:hunter2@proxy.corp:3128 failed
2026-11-17T10:00:02.000Z signed in with ` + token + `
2026-11-17T10:00:03.000Z share ` + odd + ` ok`,
		EngineLog: `2026/11/17 10:00:04 pull: Authorization: Bearer abcdefghijklmnop
2026/11/17 10:00:05 env DB_PASSWORD=hunter2 for ada.lovelace@example.org in /mnt/c/Users/Ada/proj and /home/ada`,
	}
	text := d.Text(redact.New(`C:\Users\Ada`, []string{ShareToken(shareJSON)}))
	for _, leak := range []string{"hunter2", "SECRET", odd, "abcdefghijklmnop", "ada.lovelace", `Users\Ada`, "Users/Ada", "/home/ada"} {
		if strings.Contains(text, leak) {
			t.Errorf("%q survived:\n%s", leak, text)
		}
	}
	for _, kept := range []string{"Fabo for Windows 1.2.3", "== Containers ==", "db  postgres:16  running", "== Window log ==", "== Engine log ==", `~\Downloads\x`} {
		if !strings.Contains(text, kept) {
			t.Errorf("%q missing:\n%s", kept, text)
		}
	}
	// A part that could not be read says so instead of being empty.
	text = DiagReport{EngineNote: "WSL is not running"}.Text(redact.New("", nil))
	if !strings.Contains(text, "== Engine log ==\n(not read: WSL is not running)") || strings.Contains(text, "Containers") {
		t.Fatal(text)
	}
}

// TestTailFile keeps the end of a log without its cut first line.
func TestTailFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fabo-win.log")
	os.WriteFile(path, []byte("first line\nsecond line\nthird\n"), 0o644)
	if got, _ := TailFile(path, 1<<10); got != "first line\nsecond line\nthird\n" {
		t.Fatalf("%q", got)
	}
	if got, _ := TailFile(path, 15); got != "third\n" {
		t.Fatalf("%q", got)
	}
	if _, err := TailFile(filepath.Join(t.TempDir(), "none"), 10); err == nil {
		t.Fatal("no error for a missing log")
	}
	if DropCutLine("half\nwhole\n", true) != "whole\n" || DropCutLine("half", true) != "" {
		t.Fatal("DropCutLine")
	}
}

// TestDiagHelpers covers the container list, share token and file name.
func TestDiagHelpers(t *testing.T) {
	lines, err := ContainerLines([]byte(`[{"Names":["/web"],"Image":"nginx","State":"running"},{"Names":[],"Image":"x","State":"exited"}]`))
	if err != nil || len(lines) != 2 || lines[0] != "web  nginx  running" || lines[1] != "  x  exited" {
		t.Fatal(lines, err)
	}
	if ShareToken([]byte("not json")) != "" || ShareToken([]byte(`{"token":" t "}`)) != "t" {
		t.Fatal("ShareToken")
	}
	if got := ReportName(time.Date(2026, 11, 17, 10, 30, 0, 0, time.UTC)); got != "Fabo report 2026-11-17 103000.txt" {
		t.Fatal(got)
	}
	if !strings.Contains(EngineLogScript, "tail -c 524288 /var/log/fabo.log") || !strings.Contains(EngineLogScript, "journalctl -u fabo") {
		t.Fatal(EngineLogScript)
	}
}
