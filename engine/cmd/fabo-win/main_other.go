//go:build !windows

package main

import (
	"fmt"
	"os"
)

// host exists on Windows only; elsewhere the package builds for its tests.
type host struct{}

// main says where fabo-win runs.
func main() {
	fmt.Fprintln(os.Stderr, "fabo-win runs on Windows only")
	os.Exit(1)
}
