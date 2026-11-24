//go:build !windows

package desktop

// RegisterShellMenu is a no-op outside Windows.
func RegisterShellMenu(_, _ string) error { return nil }

// UnregisterShellMenu is a no-op outside Windows.
func UnregisterShellMenu() error { return nil }
