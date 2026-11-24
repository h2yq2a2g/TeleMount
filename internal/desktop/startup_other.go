//go:build !windows

package desktop

import "errors"

type unsupportedStartupStore struct{}

// NewWindowsStartupStore returns an unsupported stub StartupStore on non-Windows platforms.
func NewWindowsStartupStore() StartupStore {
	return unsupportedStartupStore{}
}

func (unsupportedStartupStore) Get(string) (string, bool, error) {
	return "", false, errors.ErrUnsupported
}

func (unsupportedStartupStore) Set(string, string) error {
	return errors.ErrUnsupported
}

func (unsupportedStartupStore) Delete(string) error {
	return errors.ErrUnsupported
}
