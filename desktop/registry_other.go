//go:build !darwin

package main

// Linux requires a configured Docker credential helper. The Windows host
// and its native credential store remain outside the current Go port.
func newNativeRegistryCredentials() nativeRegistryCredentials { return nil }
