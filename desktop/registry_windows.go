//go:build windows

package main

import (
	"errors"
	"fmt"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Registry logins in Windows' Credential Manager, written exactly as
// Docker's own docker-credential-wincred writes them: a generic credential
// named by the server, the user name, the secret as the blob, and a
// "label" attribute of "Docker Credentials" that marks Docker's entries
// among the user's others. A docker CLI configured with
// "credsStore": "wincred" then finds the same logins.

const (
	credTypeGeneric       = 1
	credPersistLocal      = 2
	credLabelKeyword      = "label"
	credLabelValue        = "Docker Credentials"
	errorNotFound         = windows.Errno(1168) // ERROR_NOT_FOUND
	credEnumerateAllCreds = 0x1
)

// winCredential is CREDENTIALW.
type winCredential struct {
	Flags              uint32
	Type               uint32
	TargetName         *uint16
	Comment            *uint16
	LastWritten        windows.Filetime
	CredentialBlobSize uint32
	CredentialBlob     *byte
	Persist            uint32
	AttributeCount     uint32
	Attributes         *winCredentialAttribute
	TargetAlias        *uint16
	UserName           *uint16
}

// winCredentialAttribute is CREDENTIAL_ATTRIBUTEW.
type winCredentialAttribute struct {
	Keyword   *uint16
	Flags     uint32
	ValueSize uint32
	Value     *byte
}

var (
	advapi32          = windows.NewLazySystemDLL("advapi32.dll")
	procCredWrite     = advapi32.NewProc("CredWriteW")
	procCredRead      = advapi32.NewProc("CredReadW")
	procCredDelete    = advapi32.NewProc("CredDeleteW")
	procCredEnumerate = advapi32.NewProc("CredEnumerateW")
	procCredFree      = advapi32.NewProc("CredFree")
)

type wincredRegistryCredentials struct{}

// newNativeRegistryCredentials is the Credential Manager store.
func newNativeRegistryCredentials() nativeRegistryCredentials { return wincredRegistryCredentials{} }

// credError maps a Credential Manager failure.
func credError(err error) error {
	if errors.Is(err, errorNotFound) {
		return errRegistryNotFound
	}
	return fmt.Errorf("Credential Manager: %w", err)
}

// docker marks whether a credential carries Docker's label.
func (c *winCredential) docker() bool {
	attrs := unsafe.Slice(c.Attributes, c.AttributeCount)
	for _, a := range attrs {
		if a.Keyword != nil && windows.UTF16PtrToString(a.Keyword) == credLabelKeyword &&
			string(unsafe.Slice(a.Value, a.ValueSize)) == credLabelValue {
			return true
		}
	}
	return false
}

// get reads key's login.
func (wincredRegistryCredentials) get(key string) (registryLogin, error) {
	target, err := windows.UTF16PtrFromString(key)
	if err != nil {
		return registryLogin{}, err
	}
	var c *winCredential
	if ok, _, err := procCredRead.Call(uintptr(unsafe.Pointer(target)), credTypeGeneric, 0, uintptr(unsafe.Pointer(&c))); ok == 0 {
		return registryLogin{}, credError(err)
	}
	defer procCredFree.Call(uintptr(unsafe.Pointer(c)))
	if !c.docker() {
		// A credential of the same name that Docker did not write.
		return registryLogin{}, errRegistryNotFound
	}
	login := registryLogin{Secret: string(unsafe.Slice(c.CredentialBlob, c.CredentialBlobSize))}
	if c.UserName != nil {
		login.Username = windows.UTF16PtrToString(c.UserName)
	}
	return login, nil
}

// set writes key's login, replacing any.
func (wincredRegistryCredentials) set(key string, login registryLogin) error {
	target, err := windows.UTF16PtrFromString(key)
	if err != nil {
		return err
	}
	user, err := windows.UTF16PtrFromString(login.Username)
	if err != nil {
		return err
	}
	keyword, _ := windows.UTF16PtrFromString(credLabelKeyword)
	label := []byte(credLabelValue)
	attr := winCredentialAttribute{Keyword: keyword, ValueSize: uint32(len(label)), Value: &label[0]}
	secret := []byte(login.Secret)
	c := winCredential{
		Type: credTypeGeneric, TargetName: target, UserName: user, Persist: credPersistLocal,
		CredentialBlobSize: uint32(len(secret)), AttributeCount: 1, Attributes: &attr,
	}
	if len(secret) > 0 {
		c.CredentialBlob = &secret[0]
	}
	if ok, _, err := procCredWrite.Call(uintptr(unsafe.Pointer(&c)), 0); ok == 0 {
		return credError(err)
	}
	return nil
}

// remove deletes key's login; none is not an error.
func (s wincredRegistryCredentials) remove(key string) error {
	if _, err := s.get(key); errors.Is(err, errRegistryNotFound) {
		return nil
	}
	target, err := windows.UTF16PtrFromString(key)
	if err != nil {
		return err
	}
	if ok, _, err := procCredDelete.Call(uintptr(unsafe.Pointer(target)), credTypeGeneric, 0); ok == 0 {
		if err := credError(err); !errors.Is(err, errRegistryNotFound) {
			return err
		}
	}
	return nil
}

// list returns Docker's logins, registry key → user name.
func (wincredRegistryCredentials) list() (map[string]string, error) {
	items := map[string]string{}
	var count uint32
	var creds **winCredential
	if ok, _, err := procCredEnumerate.Call(0, credEnumerateAllCreds, uintptr(unsafe.Pointer(&count)), uintptr(unsafe.Pointer(&creds))); ok == 0 {
		if err := credError(err); errors.Is(err, errRegistryNotFound) {
			return items, nil
		} else {
			return nil, err
		}
	}
	defer procCredFree.Call(uintptr(unsafe.Pointer(creds)))
	for _, c := range unsafe.Slice(creds, count) {
		if c == nil || c.Type != credTypeGeneric || c.TargetName == nil || !c.docker() {
			continue
		}
		// With CRED_ENUMERATE_ALL_CREDENTIALS, generic credentials may be
		// listed as LegacyGeneric:target=<name>.
		target := strings.TrimPrefix(windows.UTF16PtrToString(c.TargetName), "LegacyGeneric:target=")
		if key, err := registryKey(target); err == nil {
			user := ""
			if c.UserName != nil {
				user = windows.UTF16PtrToString(c.UserName)
			}
			items[key] = user
		}
	}
	return items, nil
}
