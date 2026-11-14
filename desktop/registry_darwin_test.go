//go:build darwin

package main

import (
	"errors"
	"fmt"
	"os"
	"testing"
	"time"
)

// Uses one disposable .invalid registry, with no account/network login.
func TestNativeRegistryCredentialsRoundTrip(t *testing.T) {
	if os.Getenv("KIRI_KEYCHAIN_TEST") != "1" {
		t.Skip("set KIRI_KEYCHAIN_TEST=1 for the disposable Keychain check")
	}
	store := newNativeRegistryCredentials()
	key := fmt.Sprintf("fabo-desktop-check-%d.invalid", time.Now().UnixNano())
	t.Cleanup(func() {
		if err := store.remove(key); err != nil {
			t.Errorf("remove test credential: %v", err)
		}
	})
	if err := store.set(key, registryLogin{"test-user", "test-password"}); err != nil {
		t.Fatal(err)
	}
	if login, err := store.get(key); err != nil || login.Username != "test-user" || login.Secret != "test-password" {
		t.Fatalf("credential round trip failed: %v", err)
	}
	if err := store.set(key, registryLogin{"changed-user", "changed-password"}); err != nil {
		t.Fatal(err)
	}
	if accounts, err := store.list(); err != nil || accounts[key] != "changed-user" {
		t.Fatalf("attribute-only list did not include test credential: %v", err)
	}
	if err := store.remove(key); err != nil {
		t.Fatal(err)
	}
	if _, err := store.get(key); !errors.Is(err, errRegistryNotFound) {
		t.Fatalf("deleted credential was found: %v", err)
	}
}
