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
