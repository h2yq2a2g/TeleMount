//go:build windows

package desktop

import (
	"errors"
	"os"

	"golang.org/x/sys/windows/registry"
)

const runRegistrySubkey = `Software\Microsoft\Windows\CurrentVersion\Run`

type windowsStartupStore struct {
	root   registry.Key
	subkey string
}

// NewWindowsStartupStore returns a StartupStore backed by HKCU\...\Run.
func NewWindowsStartupStore() StartupStore {
	return &windowsStartupStore{
		root:   registry.CURRENT_USER,
		subkey: runRegistrySubkey,
	}
}

func (s *windowsStartupStore) Get(name string) (string, bool, error) {
	k, err := registry.OpenKey(s.root, s.subkey, registry.QUERY_VALUE)
	if errors.Is(err, registry.ErrNotExist) || errors.Is(err, os.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	defer k.Close()

	val, _, err := k.GetStringValue(name)
	if errors.Is(err, registry.ErrNotExist) || errors.Is(err, os.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return val, true, nil
}

func (s *windowsStartupStore) Set(name, value string) error {
	k, _, err := registry.CreateKey(s.root, s.subkey, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	return k.SetStringValue(name, value)
}

func (s *windowsStartupStore) Delete(name string) error {
	k, err := registry.OpenKey(s.root, s.subkey, registry.SET_VALUE)
	if errors.Is(err, registry.ErrNotExist) || errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer k.Close()

	err = k.DeleteValue(name)
	if errors.Is(err, registry.ErrNotExist) || errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
