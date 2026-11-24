//go:build windows

package desktop

import "golang.org/x/sys/windows"

// NewDriveChecker returns a Windows DriveChecker backed by GetLogicalDrives.
func NewDriveChecker() DriveChecker {
	return &bitmaskDriveChecker{getMask: windows.GetLogicalDrives}
}
