//go:build !windows

package desktop

import "errors"

// LaunchElevatedInstaller is only supported on Windows desktop.
func LaunchElevatedInstaller(_ string) error {
	return errors.New("installer auto-update is only supported on Windows")
}
