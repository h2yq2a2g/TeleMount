//go:build !windows

package desktop

import "errors"

// NewDriveChecker returns a stub DriveChecker on non-Windows platforms.
func NewDriveChecker() DriveChecker {
	return &bitmaskDriveChecker{
		getMask: func() (uint32, error) {
			return 0, errors.ErrUnsupported
		},
	}
}
