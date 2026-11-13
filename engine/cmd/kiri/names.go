//go:build linux || darwin

package main

import (
	"strings"
)

// names are what two installations on one machine must not share. A bundle
// whose identifier is dev.kiri.app.<feature> gets its own set, so a
// development build runs beside the installed app.
type names struct {
	app      string // Finder mount and data folder: "Kiri", "Kiri Dev"
	state    string // under HOME: ".kiri", ".kiri-dev"
	context  string // docker context: "kiri", "kiri-dev"
	builder  string // Buildx builder: "kiri-build", "kiri-dev-build"
	agent    string // LaunchAgent label: "dev.kiri.daemon", "dev.kiri.daemon.dev"
	id       string // bundle identifier, which names the agent in Login Items
	flavored bool   // leaves PATH, docker's default context and the domain ports to the installed app
}

const appID = "dev.kiri.app"

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

// namesFor maps a bundle identifier to its names. Anything that is not a
// well-formed feature identifier is the standard installation.
func namesFor(id string) names {
	feature, ok := strings.CutPrefix(id, appID+".")
	if !ok || feature == "" || strings.ContainsFunc(feature, func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < '0' || r > '9')
	}) {
		return names{app: "Kiri", state: ".kiri", context: "kiri", builder: "kiri-build", agent: "dev.kiri.daemon", id: appID}
	}
	return names{
		app:      "Kiri " + strings.ToUpper(feature[:1]) + feature[1:],
		state:    ".kiri-" + feature,
		context:  "kiri-" + feature,
		builder:  "kiri-" + feature + "-build",
		agent:    "dev.kiri.daemon." + feature,
		id:       appID + "." + feature,
		flavored: true,
	}
}
