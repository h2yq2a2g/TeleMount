//go:build windows

package desktop

import (
	"fmt"
	"path/filepath"

	"golang.org/x/sys/windows"
)

// LaunchElevatedInstaller launches TeleMount-Setup.exe silently with UAC elevation
// so Inno Setup can close the running app, replace Program Files\TeleMount, and relaunch minimized.
func LaunchElevatedInstaller(installerPath string) error {
	verb, err := windows.UTF16PtrFromString("runas")
	if err != nil {
		return err
	}
	file, err := windows.UTF16PtrFromString(installerPath)
	if err != nil {
		return err
	}
	params, err := windows.UTF16PtrFromString("/VERYSILENT /SUPPRESSMSGBOXES /NORESTART /CLOSEAPPLICATIONS")
	if err != nil {
		return err
	}
	dir, err := windows.UTF16PtrFromString(filepath.Dir(installerPath))
	if err != nil {
		return err
	}
	if err := windows.ShellExecute(0, verb, file, params, dir, windows.SW_SHOWNORMAL); err != nil {
		if err == windows.ERROR_CANCELLED {
			return ErrUpdateCanceled
		}
		return fmt.Errorf("launch installer: %w", err)
	}
	return nil
}
