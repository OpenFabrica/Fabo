//go:build !darwin && !windows

package main

// Linux requires a configured Docker credential helper. Windows uses its
// Credential Manager (registry_windows.go).
func newNativeRegistryCredentials() nativeRegistryCredentials { return nil }
