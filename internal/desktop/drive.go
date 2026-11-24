package desktop

import (
	"fmt"
	"strings"
)

// DriveChecker probes whether a Windows drive letter is currently occupied.
type DriveChecker interface {
	IsDriveInUse(drive string) (bool, error)
}

func parseDriveLetter(s string) (byte, error) {
	trimmed := strings.TrimSpace(s)
	if trimmed == "" {
		return 0, fmt.Errorf("drive letter cannot be empty")
	}
	if len(trimmed) == 3 && trimmed[1] == ':' && (trimmed[2] == '\\' || trimmed[2] == '/') {
		trimmed = trimmed[:2]
	}
	if len(trimmed) == 2 && trimmed[1] == ':' {
		trimmed = trimmed[:1]
	}
	if len(trimmed) != 1 {
		return 0, fmt.Errorf("invalid drive letter %q: expected format like V:", s)
	}
	ch := trimmed[0]
	if ch >= 'a' && ch <= 'z' {
		ch = ch - 'a' + 'A'
	}
	if ch < 'A' || ch > 'Z' {
		return 0, fmt.Errorf("invalid drive letter %q: must be A-Z", s)
	}
	return ch, nil
}

type bitmaskDriveChecker struct {
	getMask func() (uint32, error)
}

func (c *bitmaskDriveChecker) IsDriveInUse(drive string) (bool, error) {
	letter, err := parseDriveLetter(drive)
	if err != nil {
		return false, err
	}
	if c == nil || c.getMask == nil {
		return false, fmt.Errorf("get logical drives: drive checker not initialized")
	}
	mask, err := c.getMask()
	if err != nil {
		return false, fmt.Errorf("get logical drives: %w", err)
	}
	bit := uint(letter - 'A')
	return (mask & (uint32(1) << bit)) != 0, nil
}

// ListAvailableDrives returns selectable Windows drive letters (D:-Z:, excluding C:)
// that are currently free, always including currentDrive (normalized) so the active
// mount stays selected in dropdown UIs.
func ListAvailableDrives(checker DriveChecker, currentDrive string) []string {
	cur, _ := NormalizeDriveLetter(currentDrive)
	out := make([]string, 0, 23)
	includedCur := false
	for ch := byte('D'); ch <= 'Z'; ch++ {
		drv := string(ch) + ":"
		if drv == cur {
			out = append(out, drv)
			includedCur = true
			continue
		}
		if checker != nil {
			inUse, err := checker.IsDriveInUse(drv)
			if err == nil && inUse {
				continue
			}
		}
		out = append(out, drv)
	}
	// Preserve A: or B: if the user explicitly configured it earlier.
	if cur != "" && !includedCur {
		out = append([]string{cur}, out...)
	}
	return out
}
