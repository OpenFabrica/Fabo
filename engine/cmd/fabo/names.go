//go:build linux || darwin

package main

import (
	"strings"
)

// names are what two installations on one machine must not share. A bundle
// whose identifier is com.openfabrica.fabo.<feature> gets its own set, so a
// development build runs beside the installed app.
type names struct {
	app      string // Finder mount and data folder: "Fabo", "Fabo Dev"
	state    string // under HOME: ".fabo", ".fabo-dev"
	context  string // docker context: "fabo", "fabo-dev"
	builder  string // Buildx builder: "fabo-build", "fabo-dev-build"
	agent    string // LaunchAgent label: "com.openfabrica.fabo-daemon", "com.openfabrica.fabo-daemon.dev"
	id       string // bundle identifier, which names the agent in Login Items
	flavored bool   // leaves PATH, docker's default context and the domain ports to the installed app
}

const appID = "com.openfabrica.fabo"

// bundleID reads CFBundleIdentifier from an Info.plist's text.
func bundleID(plist string) string {
	_, rest, ok := strings.Cut(plist, "<key>CFBundleIdentifier</key>")
	if !ok {
		return ""
	}
	_, rest, ok = strings.Cut(rest, "<string>")
	if !ok {
		return ""
	}
	id, _, _ := strings.Cut(rest, "</string>")
	return strings.TrimSpace(id)
}

// plistTrue reports whether an Info.plist's text sets key to true.
func plistTrue(plist, key string) bool {
	_, rest, ok := strings.Cut(plist, "<key>"+key+"</key>")
	return ok && strings.HasPrefix(strings.TrimSpace(rest), "<true/>")
}

// namesFor maps a bundle identifier to its names. Anything that is not a
// well-formed feature identifier is the standard installation.
func namesFor(id string) names {
	feature, ok := strings.CutPrefix(id, appID+".")
	if !ok || feature == "" || strings.ContainsFunc(feature, func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < '0' || r > '9')
	}) {
		return names{app: "Fabo", state: ".fabo", context: "fabo", builder: "fabo-build", agent: "com.openfabrica.fabo-daemon", id: appID}
	}
	return names{
		app:      "Fabo " + strings.ToUpper(feature[:1]) + feature[1:],
		state:    ".fabo-" + feature,
		context:  "fabo-" + feature,
		builder:  "fabo-" + feature + "-build",
		agent:    "com.openfabrica.fabo-daemon." + feature,
		id:       appID + "." + feature,
		flavored: true,
	}
}
