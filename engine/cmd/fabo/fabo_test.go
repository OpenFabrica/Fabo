//go:build linux || darwin

package main

import (
	"testing"
)

func TestNamesFollowTheBundleIdentifier(t *testing.T) {
	plist := "<dict><key>CFBundleName</key><string>Fabo Go</string>\n<key>CFBundleIdentifier</key><string> com.openfabrica.fabo.go </string></dict>"
	if id := bundleID(plist); id != "com.openfabrica.fabo.go" {
		t.Fatalf("bundleID = %q", id)
	}
	standard := names{app: "Fabo", state: ".fabo", context: "fabo", builder: "fabo-build", agent: "com.openfabrica.fabo-daemon", id: "com.openfabrica.fabo"}
	for id, want := range map[string]names{
		"":                            standard,
		"com.openfabrica.fabo":        standard,
		"com.openfabrica.fabo.":       standard,
		"com.openfabrica.fabo.My-App": standard,
		"com.openfabrica.fabo.dev":    {app: "Fabo Dev", state: ".fabo-dev", context: "fabo-dev", builder: "fabo-dev-build", agent: "com.openfabrica.fabo-daemon.dev", id: "com.openfabrica.fabo.dev", flavored: true},
		"com.openfabrica.fabo.go":     {app: "Fabo Go", state: ".fabo-go", context: "fabo-go", builder: "fabo-go-build", agent: "com.openfabrica.fabo-daemon.go", id: "com.openfabrica.fabo.go", flavored: true},
	} {
		if got := namesFor(id); got != want {
			t.Errorf("namesFor(%q) = %+v, want %+v", id, got, want)
		}
	}
}

// Only a literal true turns a bundle key on.
func TestPlistTrueReadsBooleanKeys(t *testing.T) {
	plist := "<dict><key>FaboSharesEngine</key>\n  <true/><key>Other</key><false/></dict>"
	if !plistTrue(plist, "FaboSharesEngine") || plistTrue(plist, "Other") || plistTrue(plist, "Missing") {
		t.Fatal("plistTrue misread the keys")
	}
}
