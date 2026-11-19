package main

import (
	"embed"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"

	"github.com/egoist/mygo"
	// MyGo's own strings (controls, dialogs, menus) for the languages it
	// ships; without these they stay English inside a translated window.
	_ "github.com/egoist/mygo/locales/ja"
	_ "github.com/egoist/mygo/locales/zh-Hans"
	_ "github.com/egoist/mygo/locales/zh-Hant"
)

// English keys match tr's second argument. Catalogs are embedded in every
// desktop build; missing messages retain the English text.
//
//go:embed languages/*.json
var languageFiles embed.FS

var languages = []struct{ tag, name string }{
	{"en", "English"}, {"zh-Hans", "简体中文"}, {"zh-Hant", "繁體中文"},
	{"de", "Deutsch"}, {"es", "Español"}, {"fr", "Français"},
	{"pt-BR", "Português (Brasil)"}, {"it", "Italiano"}, {"pl", "Polski"},
	{"ru", "Русский"}, {"tr", "Türkçe"}, {"ja", "日本語"}, {"ko", "한국어"},
	{"vi", "Tiếng Việt"}, {"id", "Bahasa Indonesia"},
}
var languageWords = loadLanguageWords()

func loadLanguageWords() map[string]map[string]string {
	all := make(map[string]map[string]string)
	for _, language := range languages {
		if language.tag == "en" {
			continue
		}
		data, err := languageFiles.ReadFile("languages/" + language.tag + ".json")
		if err != nil {
			panic(err)
		}
		var words map[string]string
		if err := json.Unmarshal(data, &words); err != nil {
			panic(fmt.Errorf("language %s: %w", language.tag, err))
		}
		all[language.tag] = words
	}
	return all
}

// languageFor follows Fabo's tag rules, including legacy persisted "zh".
func languageFor(tag string) string {
	tag = strings.ToLower(strings.TrimSpace(tag))
	tag = strings.ReplaceAll(strings.Split(strings.Split(tag, ".")[0], "@")[0], "_", "-")
	is := func(id string) bool { return tag == id || strings.HasPrefix(tag, id+"-") }
	if is("zh") {
		if !is("zh-hans") && (is("zh-hant") || is("zh-tw") || is("zh-hk") || is("zh-mo")) {
			return "zh-Hant"
		}
		return "zh-Hans"
	}
	if is("pt") {
		return "pt-BR"
	}
	if is("in") {
		return "id"
	}
	for _, language := range languages {
		if is(strings.ToLower(language.tag)) {
			return language.tag
		}
	}
	return ""
}
func systemLanguage(preferred []string) string {
	for _, tag := range preferred {
		if language := languageFor(tag); language != "" {
			return language
		}
	}
	return "en"
}
func (p prefs) language(locale string) string {
	if p.Lang != "" && p.Lang != "system" {
		if language := languageFor(p.Lang); language != "" {
			return language
		}
		return "en"
	}
	return systemLanguage([]string{locale})
}
func languageName(tag string) string {
	for _, language := range languages {
		if language.tag == tag {
			return language.name
		}
	}
	return "English"
}

// lang is set before building a frame so all of its strings agree. It is
// atomic because background work phrases messages too: the Debug page's
// session goroutines report their events as they happen.
var lang atomic.Value

// currentLang is the language of the frame being built.
func currentLang() string {
	if v, ok := lang.Load().(string); ok {
		return v
	}
	return "en"
}

func tr(zh, en string) string { return textFor(currentLang(), zh, en) }
func textFor(language, zh, en string) string {
	if language == "zh" || language == "zh-Hans" {
		return zh
	}
	if text := languageWords[language][en]; text != "" {
		return text
	}
	return en
}

// Keep MyGo's native controls and menus in the same language as the window.
// desktop.json remains the source of the application's language preference.
func (s *desktop) applyLanguage() {
	if s.prefs.Lang == "" || s.prefs.Lang == "system" {
		mygo.App.SetLocale("")
	} else {
		mygo.App.SetLocale(s.prefs.language("en"))
	}
}
