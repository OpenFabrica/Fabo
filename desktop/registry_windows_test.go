//go:build windows

package main

import (
	"errors"
	"fmt"
	"os"
	"testing"
	"time"
)

// TestNativeRegistryCredentialsRoundTrip uses one disposable .invalid
// registry in this user's Credential Manager.
func TestNativeRegistryCredentialsRoundTrip(t *testing.T) {
	if os.Getenv("FABO_WINCRED_TEST") != "1" {
		t.Skip("set FABO_WINCRED_TEST=1 for the disposable Credential Manager check")
	}
	store := newNativeRegistryCredentials()
	key := fmt.Sprintf("fabo-desktop-check-%d.invalid", time.Now().UnixNano())
	t.Cleanup(func() { _ = store.remove(key) })
	if err := store.set(key, registryLogin{"test-user", "test-password"}); err != nil {
		t.Fatal(err)
	}
	if login, err := store.get(key); err != nil || login.Username != "test-user" || login.Secret != "test-password" {
		t.Fatalf("round trip: %+v, %v", login, err)
	}
	if err := store.set(key, registryLogin{"changed-user", "changed-password"}); err != nil {
		t.Fatal(err)
	}
	if accounts, err := store.list(); err != nil || accounts[key] != "changed-user" {
		t.Fatalf("list: %v, %v", accounts[key], err)
	}
	if err := store.remove(key); err != nil {
		t.Fatal(err)
	}
	if _, err := store.get(key); !errors.Is(err, errRegistryNotFound) {
		t.Fatalf("deleted credential was found: %v", err)
	}
}
