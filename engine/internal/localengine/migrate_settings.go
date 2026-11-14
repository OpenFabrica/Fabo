//go:build linux || darwin

package localengine

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
)

func migrateKiriSettings(path string, state *State, validateVM func(VMSettings) error) ([]string, error) {
	if path == "" {
		return nil, nil
	}
	source, err := os.OpenRoot(path)
	if err != nil {
		return nil, fmt.Errorf("open Kiri daemon settings: %w", err)
	}
	defer source.Close()

	var copied []string
	if validateVM != nil {
		var vm VMSettings
		if found, err := readKiriSetting(source, "vm.json", &vm); err != nil {
			return nil, err
		} else if found {
			// The Kiri file has no stop flag. Import only its resource settings.
			vm.Stopped = false
			if err := validateVM(vm); err != nil {
				return nil, fmt.Errorf("migrate VM settings: %w", err)
			}
			if err := state.SaveVMSettings(vm); err != nil {
				return nil, err
			}
			copied = append(copied, "vm")
		}
	}
	var builder builderSettings
	if found, err := readKiriSetting(source, builderSettingsFile, &builder); err != nil {
		return nil, err
	} else if found {
		if err := builder.validate(); err != nil {
			return nil, fmt.Errorf("migrate builder settings: %w", err)
		}
		if err := saveJSON(state.dir, builderSettingsFile, builder); err != nil {
			return nil, err
		}
		copied = append(copied, "builder")
	}
	proxy := proxyConfig{Mode: "system"}
	if found, err := readKiriSetting(source, "proxy.json", &proxy); err != nil {
		return nil, err
	} else if found {
		if err := proxy.validate(); err != nil {
			return nil, fmt.Errorf("migrate proxy settings: %w", err)
		}
		if err := saveJSON(state.dir, "proxy.json", proxy); err != nil {
			return nil, err
		}
		copied = append(copied, "proxy")
	}
	ports := domainPorts{HTTP: domainDefaults[0], HTTPS: domainDefaults[1]}
	if found, err := readKiriSetting(source, "domains.json", &ports); err != nil {
		return nil, err
	} else if found {
		if err := ports.validate(); err != nil {
			return nil, fmt.Errorf("migrate domain ports: %w", err)
		}
		if err := saveJSON(state.dir, "domains.json", ports); err != nil {
			return nil, err
		}
		copied = append(copied, "domains")
	}
	return copied, nil
}

// Limit reads to regular JSON files in the explicitly selected configuration
// directory. Missing files retain the Go daemon's defaults.
func readKiriSetting(source *os.Root, name string, out any) (bool, error) {
	const maxSize = 64 << 10
	fail := func(err error) (bool, error) { return false, fmt.Errorf("read Kiri setting %s: %w", name, err) }
	info, err := source.Stat(name)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return fail(err)
	}
	if !info.Mode().IsRegular() || info.Size() > maxSize {
		return fail(errors.New("expected a regular JSON file up to 64 KiB"))
	}
	f, err := source.Open(name)
	if err != nil {
		return fail(err)
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, maxSize+1))
	if err != nil {
		return fail(err)
	}
	if len(raw) > maxSize {
		return fail(errors.New("expected a regular JSON file up to 64 KiB"))
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fail(err)
	}
	return true, nil
}
