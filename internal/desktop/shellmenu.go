package desktop

import (
	"fmt"
	"strings"
)

// ShellMenuSpec describes one Windows Explorer context-menu verb under the TeleMount cascade menu.
type ShellMenuSpec struct {
	Key    string
	Label  string
	Action string
}

// ShellMenuSpecs returns the localized OneDrive-style right-click actions for TeleMount drives.
// Defaults to English ("en"); supports Vietnamese ("vi").
func ShellMenuSpecs(lang string) []ShellMenuSpec {
	if strings.EqualFold(strings.TrimSpace(lang), "vi") {
		return []ShellMenuSpec{
			{Key: "01_pin", Label: "Luôn giữ trên thiết bị này (Offline)", Action: "pin"},
			{Key: "02_free", Label: "Giải phóng dung lượng (Chỉ giữ trên Cloud)", Action: "free"},
			{Key: "03_sync", Label: "Đồng bộ ngay", Action: "sync"},
		}
	}
	return []ShellMenuSpec{
		{Key: "01_pin", Label: "Always keep on this device", Action: "pin"},
		{Key: "02_free", Label: "Free up space", Action: "free"},
		{Key: "03_sync", Label: "Sync now", Action: "sync"},
	}
}

// DefaultShellMenuSpecs returns the default (English) OneDrive-style right-click actions.
func DefaultShellMenuSpecs() []ShellMenuSpec {
	return ShellMenuSpecs("en")
}

// MapWindowsPathToVirtual validates that winPath resides on driveLetter (e.g. "V:")
// and maps it to a canonical virtual path ("/" or "/Docs/file.txt").
func MapWindowsPathToVirtual(winPath, driveLetter string) (string, error) {
	drv, err := NormalizeDriveLetter(driveLetter)
	if err != nil {
		return "", err
	}
	// Trim whitespace and stray trailing quote from Windows Explorer %V root ("V:\"").
	raw := strings.TrimSpace(winPath)
	raw = strings.Trim(raw, `"`)
	if raw == "" {
		return "", fmt.Errorf("empty path")
	}
	if len(raw) < 2 || raw[1] != ':' {
		return "", fmt.Errorf("path %q is not on drive %s", winPath, drv)
	}
	gotDrive := strings.ToUpper(raw[:2])
	if gotDrive != drv {
		return "", fmt.Errorf("path %q is on drive %s, not mounted TeleMount drive %s", winPath, gotDrive, drv)
	}
	rest := raw[2:]
	if rest != "" && rest[0] != '\\' && rest[0] != '/' {
		return "", fmt.Errorf("invalid drive-relative path %q", winPath)
	}
	if strings.ContainsRune(rest, 0) {
		return "", fmt.Errorf("path contains null byte")
	}
	for _, seg := range strings.Split(strings.ReplaceAll(rest, `\`, `/`), "/") {
		if seg == ".." {
			return "", fmt.Errorf("path %q must not contain parent traversal", winPath)
		}
	}
	return NormalizeVirtualPath(rest), nil
}

func shellAppliesToForItems(driveLetter string) string {
	return fmt.Sprintf(`System.ParsingPath:~< "%s\"`, driveLetter)
}

func shellAppliesToForBackground(driveLetter string) string {
	return fmt.Sprintf(`System.ItemFolderPathDisplay:~< "%s\"`, driveLetter)
}
