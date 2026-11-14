//go:build e2e

package main

// Opt-in, window-scoped automation for native desktop acceptance checks.
// Production builds do not import or expose this socket.
import _ "github.com/egoist/mygo/plugins/automation"
