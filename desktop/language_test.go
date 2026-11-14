package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"unicode"

	"github.com/egoist/mygo/ui"
)

func TestLanguageResolution(t *testing.T) {
	for _, tc := range []struct{ tag, want string }{
		{"zh", "zh-Hans"}, {"zh_CN.UTF-8@pinyin", "zh-Hans"}, {"zh-TW", "zh-Hant"},
		{"zh-HK", "zh-Hant"}, {"zh-MO", "zh-Hant"}, {"zh-Hant-CN", "zh-Hant"},
		{"zh-Hans-TW", "zh-Hans"}, {"pt-PT", "pt-BR"}, {"pt_BR", "pt-BR"},
		{"in-ID", "id"}, {"fr-CA", "fr"}, {"EN_gb", "en"}, {"de-AT", "de"},
		{"ja-JP", "ja"}, {"ko-KR", "ko"}, {"nl-NL", ""}, {"zhx", ""}, {"", ""},
	} {
		if got := languageFor(tc.tag); got != tc.want {
			t.Errorf("%s: got %s, want %s", tc.tag, got, tc.want)
		}
	}
	for _, tc := range []struct {
		preferred []string
		want      string
	}{
		{[]string{"nl-NL", "fr-CA"}, "fr"}, {[]string{"en-GB", "zh-TW"}, "en"},
		{[]string{"nl", "ar"}, "en"}, {nil, "en"},
	} {
		if got := systemLanguage(tc.preferred); got != tc.want {
			t.Errorf("%v: %s", tc.preferred, got)
		}
	}
	if (prefs{Lang: "invalid"}).language("zh") != "en" {
		t.Fatal("unknown explicit language must fall back to English")
	}
	path := filepath.Join(t.TempDir(), "desktop.json")
	prefs{Lang: "zh"}.save(path)
	if loadPrefs(path).language("en") != "zh-Hans" {
		t.Fatal("legacy Chinese preference lost")
	}
	if got := textFor("fr", "中文", "an unknown future message"); got != "an unknown future message" {
		t.Fatal(got)
	}
}

// Include literal calls and the bilingual entries used by navigation,
// settings and accent choices, so newly added UI text cannot silently fall
// back to English in the shipped languages.
func desktopMessages(t *testing.T) map[string]string {
	t.Helper()
	messages := map[string]string{}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	literal := func(e ast.Expr) string {
		if b, ok := e.(*ast.BasicLit); ok && b.Kind == token.STRING {
			s, _ := strconv.Unquote(b.Value)
			return s
		}
		return ""
	}
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") || path == "language.go" {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.CallExpr:
				id, ok := x.Fun.(*ast.Ident)
				if !ok {
					break
				}
				offset := 0
				if id.Name == "textFor" {
					offset = 1
				} else if id.Name != "tr" {
					break
				}
				if len(x.Args) == 2+offset {
					zh, en := literal(x.Args[offset]), literal(x.Args[offset+1])
					if en != "" {
						messages[en] = zh
					}
				}
			case *ast.CompositeLit:
				for i := 0; i+1 < len(x.Elts); i++ {
					zh, en := literal(x.Elts[i]), literal(x.Elts[i+1])
					if en != "" && strings.ContainsFunc(zh, func(r rune) bool { return unicode.Is(unicode.Han, r) }) {
						messages[en] = zh
					}
				}
			}
			return true
		})
	}
	return messages
}

var printfSlot = regexp.MustCompile(`%(?:\[(\d+)\])?[-+ #0]*\d*(?:\.\d+)?(?:\[(\d+)\])?([sdvf%])`)

func formatSlots(format string) map[int]string {
	slots := map[int]string{}
	next := 1
	for _, m := range printfSlot.FindAllStringSubmatch(format, -1) {
		if m[3] == "%" {
			continue
		}
		if m[1] != "" {
			next, _ = strconv.Atoi(m[1])
		}
		if m[2] != "" {
			next, _ = strconv.Atoi(m[2])
		}
		slots[next] = m[3]
		next++
	}
	return slots
}
func TestShippedLanguageCatalogsCoverDesktopAndPreserveArguments(t *testing.T) {
	messages := desktopMessages(t)
	if len(languages) != 15 {
		t.Fatalf("languages: %d", len(languages))
	}
	for _, language := range languages {
		if language.tag == "en" {
			continue
		}
		words := languageWords[language.tag]
		for en, zh := range messages {
			text := words[en]
			if text == "" {
				t.Errorf("%s missing %q", language.tag, en)
				continue
			}
			if language.tag == "zh-Hans" && text != zh {
				t.Errorf("Chinese catalog differs from source for %q", en)
			}
			slots := formatSlots(en)
			if got := formatSlots(text); !reflect.DeepEqual(got, slots) {
				t.Errorf("%s %q: slots %v, want %v", language.tag, en, got, slots)
				continue
			}
			if len(slots) > 0 {
				args := make([]any, len(slots))
				for i, verb := range slots {
					switch verb {
					case "d":
						args[i-1] = 3
					case "f":
						args[i-1] = 3.5
					default:
						args[i-1] = "sample"
					}
				}
				if result := fmt.Sprintf(text, args...); strings.Contains(result, "%!") {
					t.Errorf("%s %q: %s", language.tag, en, result)
				}
			}
			if strings.Contains(text, "{error}") || strings.Contains(text, "<span") {
				t.Errorf("unadapted upstream text %s %q", language.tag, en)
			}
		}
	}
}

func TestEveryLanguageRendersAndPersists(t *testing.T) {
	for _, language := range languages {
		t.Run(language.tag, func(t *testing.T) {
			_, s := newFakeEngine(t)
			s.prefsFile = filepath.Join(t.TempDir(), "desktop.json")
			s.prefs.Lang = language.tag
			s.navigate(settingsPage)
			tt := ui.NewTester(s.view, 1180, 900)
			if !tt.HasText(textFor(language.tag, "外观", "Appearance")) {
				t.Fatal("settings not translated")
			}
			if !tt.HasText(language.name) {
				t.Fatal("selected language not shown in its own name")
			}
			s.savePrefs()
			if got := loadPrefs(s.prefsFile).language("en"); got != language.tag {
				t.Fatalf("saved %s", got)
			}
			s.navigate(containersPage)
			tt.Frame()
			if !tt.HasText(textFor(language.tag, "运行容器", "Run container")) {
				t.Fatal("container actions not translated")
			}
		})
	}
}
