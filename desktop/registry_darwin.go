//go:build darwin

package main

import (
	"errors"
	"fmt"
	"sync"
	"unsafe"

	"github.com/ebitengine/purego"
)

// Like MyGo's own macOS bindings, load the system frameworks through
// purego. The desktop retains its CGO_ENABLED=0 packaging contract.
type registrySecurity struct {
	err                                       error
	constants                                 map[string]uintptr
	keyCallbacks, valueCallbacks              uintptr
	dictNew                                   func(uintptr, int, uintptr, uintptr) uintptr
	dictSet                                   func(uintptr, uintptr, uintptr)
	dictGet                                   func(uintptr, uintptr) uintptr
	stringNew                                 func(uintptr, *byte, int, uint32, bool) uintptr
	stringLength                              func(uintptr) int
	stringMax                                 func(int, uint32) int
	stringCopy                                func(uintptr, *byte, int, uint32) bool
	dataNew                                   func(uintptr, *byte, int) uintptr
	dataLength                                func(uintptr) int
	dataBytes                                 func(uintptr) unsafe.Pointer
	arrayCount                                func(uintptr) int
	arrayGet                                  func(uintptr, int) uintptr
	typeID                                    func(uintptr) uintptr
	dictType, arrayType, stringType, dataType func() uintptr
	release                                   func(uintptr)
	add                                       func(uintptr, *uintptr) int32
	update                                    func(uintptr, uintptr) int32
	remove                                    func(uintptr) int32
	matching                                  func(uintptr, *uintptr) int32
}

var registrySecurityAPI = sync.OnceValue(func() *registrySecurity {
	k := &registrySecurity{constants: map[string]uintptr{}}
	cf, err := purego.Dlopen("/System/Library/Frameworks/CoreFoundation.framework/CoreFoundation", purego.RTLD_GLOBAL|purego.RTLD_NOW)
	if err != nil {
		k.err = err
		return k
	}
	security, err := purego.Dlopen("/System/Library/Frameworks/Security.framework/Security", purego.RTLD_GLOBAL|purego.RTLD_NOW)
	if err != nil {
		k.err = err
		return k
	}
	bind := func(lib uintptr, name string, target any) {
		if k.err != nil {
			return
		}
		address, err := purego.Dlsym(lib, name)
		if err != nil {
			k.err = err
			return
		}
		purego.RegisterFunc(target, address)
	}
	for name, target := range map[string]any{
		"CFDictionaryCreateMutable": &k.dictNew, "CFDictionarySetValue": &k.dictSet, "CFDictionaryGetValue": &k.dictGet,
		"CFStringCreateWithBytes": &k.stringNew, "CFStringGetLength": &k.stringLength, "CFStringGetMaximumSizeForEncoding": &k.stringMax, "CFStringGetCString": &k.stringCopy,
		"CFDataCreate": &k.dataNew, "CFDataGetLength": &k.dataLength, "CFDataGetBytePtr": &k.dataBytes,
		"CFArrayGetCount": &k.arrayCount, "CFArrayGetValueAtIndex": &k.arrayGet, "CFGetTypeID": &k.typeID,
		"CFDictionaryGetTypeID": &k.dictType, "CFArrayGetTypeID": &k.arrayType, "CFStringGetTypeID": &k.stringType, "CFDataGetTypeID": &k.dataType, "CFRelease": &k.release,
	} {
		bind(cf, name, target)
	}
	for name, target := range map[string]any{"SecItemAdd": &k.add, "SecItemUpdate": &k.update, "SecItemDelete": &k.remove, "SecItemCopyMatching": &k.matching} {
		bind(security, name, target)
	}
	symbol := func(lib uintptr, name string) uintptr {
		if k.err != nil {
			return 0
		}
		v, e := purego.Dlsym(lib, name)
		if e != nil {
			k.err = e
		}
		return v
	}
	k.keyCallbacks = symbol(cf, "kCFTypeDictionaryKeyCallBacks")
	k.valueCallbacks = symbol(cf, "kCFTypeDictionaryValueCallBacks")
	// Exported CF constants hold pointers; callback exports above are structs.
	// This is the same pointer-to-pointer read used by MyGo's constString.
	constant := func(lib uintptr, name string) {
		p := symbol(lib, name)
		if p != 0 {
			k.constants[name] = **(**uintptr)(unsafe.Pointer(&p))
		}
	}
	constant(cf, "kCFBooleanTrue")
	for _, name := range []string{"kSecClass", "kSecClassInternetPassword", "kSecAttrLabel", "kSecAttrServer", "kSecAttrPath", "kSecAttrProtocol", "kSecAttrProtocolHTTPS", "kSecAttrAccount", "kSecValueData", "kSecReturnAttributes", "kSecReturnData", "kSecMatchLimit", "kSecMatchLimitOne", "kSecMatchLimitAll"} {
		constant(security, name)
	}
	return k
})

const cfUTF8 = 0x08000100
const keychainNotFound = -25300

func (k *registrySecurity) dictionary() uintptr {
	return k.dictNew(0, 0, k.keyCallbacks, k.valueCallbacks)
}
func (k *registrySecurity) put(d uintptr, key string, value uintptr) {
	k.dictSet(d, k.constants[key], value)
}
func (k *registrySecurity) string(s string) uintptr {
	raw := []byte(s)
	return k.stringNew(0, unsafe.SliceData(raw), len(raw), cfUTF8, false)
}
func (k *registrySecurity) text(value uintptr) string {
	if value == 0 || k.typeID(value) != k.stringType() {
		return ""
	}
	raw := make([]byte, k.stringMax(k.stringLength(value), cfUTF8)+1)
	if !k.stringCopy(value, unsafe.SliceData(raw), len(raw), cfUTF8) {
		return ""
	}
	for i, b := range raw {
		if b == 0 {
			return string(raw[:i])
		}
	}
	return ""
}
func (k *registrySecurity) putString(d uintptr, key, value string) {
	v := k.string(value)
	defer k.release(v)
	k.put(d, key, v)
}
func (k *registrySecurity) query(key string) uintptr {
	q := k.dictionary()
	k.put(q, "kSecClass", k.constants["kSecClassInternetPassword"])
	k.putString(q, "kSecAttrLabel", "Docker Credentials")
	if key != "" {
		host := key
		if key == dockerHub {
			host = "index.docker.io"
		}
		k.putString(q, "kSecAttrServer", host)
	}
	return q
}
func keychainStatus(status int32) error {
	if status == 0 {
		return nil
	}
	if status == keychainNotFound {
		return errRegistryNotFound
	}
	return fmt.Errorf("Keychain error %d", status)
}
func (k *registrySecurity) match(key string, secret bool) (uintptr, error) {
	q := k.query(key)
	defer k.release(q)
	k.put(q, "kSecReturnAttributes", k.constants["kCFBooleanTrue"])
	if secret {
		k.put(q, "kSecReturnData", k.constants["kCFBooleanTrue"])
	}
	limit := "kSecMatchLimitOne"
	if key == "" {
		limit = "kSecMatchLimitAll"
	}
	k.put(q, "kSecMatchLimit", k.constants[limit])
	var result uintptr
	if err := keychainStatus(k.matching(q, &result)); err != nil {
		return 0, err
	}
	return result, nil
}

type keychainRegistryCredentials struct{}

func newNativeRegistryCredentials() nativeRegistryCredentials { return keychainRegistryCredentials{} }
func (keychainRegistryCredentials) get(key string) (registryLogin, error) {
	k := registrySecurityAPI()
	if k.err != nil {
		return registryLogin{}, k.err
	}
	item, err := k.match(key, true)
	if err != nil {
		return registryLogin{}, err
	}
	defer k.release(item)
	if item == 0 || k.typeID(item) != k.dictType() {
		return registryLogin{}, errors.New("invalid Keychain credential")
	}
	data := k.dictGet(item, k.constants["kSecValueData"])
	if data == 0 || k.typeID(data) != k.dataType() {
		return registryLogin{}, errors.New("invalid Keychain credential data")
	}
	secret := unsafe.Slice((*byte)(k.dataBytes(data)), k.dataLength(data))
	return registryLogin{k.text(k.dictGet(item, k.constants["kSecAttrAccount"])), string(secret)}, nil
}
func (keychainRegistryCredentials) set(key string, login registryLogin) error {
	k := registrySecurityAPI()
	if k.err != nil {
		return k.err
	}
	q := k.query(key)
	defer k.release(q)
	attrs := k.dictionary()
	defer k.release(attrs)
	path := ""
	if key == dockerHub {
		path = "/v1/"
	}
	k.putString(attrs, "kSecAttrAccount", login.Username)
	k.putString(attrs, "kSecAttrPath", path)
	k.put(attrs, "kSecAttrProtocol", k.constants["kSecAttrProtocolHTTPS"])
	raw := []byte(login.Secret)
	data := k.dataNew(0, unsafe.SliceData(raw), len(raw))
	defer k.release(data)
	k.put(attrs, "kSecValueData", data)
	status := k.update(q, attrs)
	if status == keychainNotFound {
		for _, key := range []string{"kSecClass", "kSecAttrLabel", "kSecAttrServer"} {
			k.put(attrs, key, k.dictGet(q, k.constants[key]))
		}
		status = k.add(attrs, nil)
	}
	return keychainStatus(status)
}
func (keychainRegistryCredentials) remove(key string) error {
	k := registrySecurityAPI()
	if k.err != nil {
		return k.err
	}
	q := k.query(key)
	defer k.release(q)
	err := keychainStatus(k.remove(q))
	if errors.Is(err, errRegistryNotFound) {
		return nil
	}
	return err
}
func (keychainRegistryCredentials) list() (map[string]string, error) {
	k := registrySecurityAPI()
	if k.err != nil {
		return nil, k.err
	}
	items := map[string]string{}
	result, err := k.match("", false)
	if errors.Is(err, errRegistryNotFound) {
		return items, nil
	}
	if err != nil {
		return nil, err
	}
	defer k.release(result)
	if result == 0 || k.typeID(result) != k.arrayType() {
		return nil, errors.New("invalid Keychain credential list")
	}
	for i := 0; i < k.arrayCount(result); i++ {
		item := k.arrayGet(result, i)
		if item == 0 || k.typeID(item) != k.dictType() {
			continue
		}
		host := k.text(k.dictGet(item, k.constants["kSecAttrServer"]))
		if host == "" {
			continue
		}
		if key, e := registryKey(host); e == nil {
			items[key] = k.text(k.dictGet(item, k.constants["kSecAttrAccount"]))
		}
	}
	return items, nil
}
